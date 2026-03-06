package alerts

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"hivemq-canary/internal/config"
	"hivemq-canary/internal/metrics"
)

// --- Mock types ---

// mockHTTPPoster records every call made to Post and returns responses from a
// pre-loaded queue. If the queue is exhausted it returns a 200 OK.
type mockHTTPPoster struct {
	mu       sync.Mutex
	calls    int
	requests []mockPostRequest
	// responses is consumed in order; the last entry is repeated indefinitely.
	responses []mockPostResponse
}

type mockPostRequest struct {
	url         string
	contentType string
	body        []byte
}

type mockPostResponse struct {
	statusCode int
	err        error
}

func (m *mockHTTPPoster) Post(url, contentType string, body io.Reader) (*http.Response, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	buf, _ := io.ReadAll(body)
	m.requests = append(m.requests, mockPostRequest{
		url:         url,
		contentType: contentType,
		body:        buf,
	})

	idx := m.calls
	if idx >= len(m.responses) {
		idx = len(m.responses) - 1
	}
	m.calls++

	resp := m.responses[idx]
	if resp.err != nil {
		return nil, resp.err
	}
	return &http.Response{
		StatusCode: resp.statusCode,
		Body:       io.NopCloser(bytes.NewReader(nil)),
	}, nil
}

func (m *mockHTTPPoster) callCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls
}

func (m *mockHTTPPoster) lastBody() []byte {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.requests) == 0 {
		return nil
	}
	return m.requests[len(m.requests)-1].body
}

// mockMetricsReader returns a fixed Snapshot from SnapshotSince and zero
// values for all other MetricsReader methods.
type mockMetricsReader struct {
	snapshot metrics.Snapshot
}

func (m *mockMetricsReader) All() []metrics.Sample                          { return nil }
func (m *mockMetricsReader) Since(t time.Time) []metrics.Sample             { return nil }
func (m *mockMetricsReader) Latest() *metrics.Sample                        { return nil }
func (m *mockMetricsReader) Count() int                                     { return 0 }
func (m *mockMetricsReader) SnapshotSince(t time.Time) metrics.Snapshot    { return m.snapshot }

// --- Helpers ---

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func newOKResponse() mockPostResponse  { return mockPostResponse{statusCode: http.StatusOK} }
func new429Response() mockPostResponse { return mockPostResponse{statusCode: http.StatusTooManyRequests} }
func new500Response() mockPostResponse { return mockPostResponse{statusCode: http.StatusInternalServerError} }

// singleRuleEngine builds a minimal Engine with one alert rule and the
// provided poster and buffer. The webhook URL is set so that postToTeams does
// not short-circuit.
func singleRuleEngine(rule config.AlertRuleConfig, snap metrics.Snapshot, poster HTTPPoster) *Engine {
	cfg := config.AlertsConfig{
		TeamsWebhookURL: "https://example.webhook/post",
		Rules:           []config.AlertRuleConfig{rule},
	}
	buf := &mockMetricsReader{snapshot: snap}
	return NewEngineWithPoster(cfg, "broker.example.com", buf, discardLogger(), poster)
}

func defaultRule() config.AlertRuleConfig {
	return config.AlertRuleConfig{
		Name:      "test-rule",
		Condition: "connection_failures",
		Operator:  ">",
		Threshold: 2,
		Severity:  "warning",
	}
}

// --- Tests ---

// TestExtractMetric verifies that every recognised condition string maps to the
// correct field on a Snapshot.
func TestExtractMetric(t *testing.T) {
	snap := metrics.Snapshot{
		ConsecutiveFailures: 7,
		RoundTripP95Ms:      42.5,
		RoundTripAvgMs:      20.1,
		DeliverySuccessRate: 0.98,
		ReconnectCount:      3,
		UptimePercent:       99.5,
	}

	engine := singleRuleEngine(defaultRule(), snap, &mockHTTPPoster{
		responses: []mockPostResponse{newOKResponse()},
	})

	cases := []struct {
		condition string
		want      float64
	}{
		{"connection_failures", 7},
		{"round_trip_p95_ms", 42.5},
		{"round_trip_avg_ms", 20.1},
		{"delivery_success_rate", 0.98},
		{"reconnect_count", 3},
		{"uptime_percent", 99.5},
	}

	for _, tc := range cases {
		t.Run(tc.condition, func(t *testing.T) {
			got := engine.extractMetric(tc.condition, &snap)
			if got != tc.want {
				t.Errorf("extractMetric(%q) = %v, want %v", tc.condition, got, tc.want)
			}
		})
	}
}

// TestExtractMetric_Unknown verifies that an unrecognised condition returns 0
// and does not panic.
func TestExtractMetric_Unknown(t *testing.T) {
	snap := metrics.Snapshot{ConsecutiveFailures: 5}
	engine := singleRuleEngine(defaultRule(), snap, &mockHTTPPoster{
		responses: []mockPostResponse{newOKResponse()},
	})

	got := engine.extractMetric("nonexistent_metric", &snap)
	if got != 0 {
		t.Errorf("extractMetric(unknown) = %v, want 0", got)
	}
}

// TestIsBreached covers every operator with a value above, exactly at, and
// below the threshold.
func TestIsBreached(t *testing.T) {
	engine := singleRuleEngine(defaultRule(), metrics.Snapshot{}, &mockHTTPPoster{
		responses: []mockPostResponse{newOKResponse()},
	})

	threshold := 10.0

	cases := []struct {
		name     string
		operator string
		value    float64
		want     bool
	}{
		// ">" operator
		{">: above threshold", ">", 11, true},
		{">: at threshold", ">", 10, false},
		{">: below threshold", ">", 9, false},

		// ">=" operator
		{">=: above threshold", ">=", 11, true},
		{">=: at threshold", ">=", 10, true},
		{">=: below threshold", ">=", 9, false},

		// "<" operator
		{"<: below threshold", "<", 9, true},
		{"<: at threshold", "<", 10, false},
		{"<: above threshold", "<", 11, false},

		// "<=" operator
		{"<=: below threshold", "<=", 9, true},
		{"<=: at threshold", "<=", 10, true},
		{"<=: above threshold", "<=", 11, false},

		// "==" operator
		{"==: equal", "==", 10, true},
		{"==: not equal above", "==", 11, false},
		{"==: not equal below", "==", 9, false},

		// unknown operator falls back to ">"
		{"unknown: above", "??", 11, true},
		{"unknown: at", "??", 10, false},
		{"unknown: below", "??", 9, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rule := config.AlertRuleConfig{
				Operator:  tc.operator,
				Threshold: threshold,
			}
			got := engine.isBreached(rule, tc.value)
			if got != tc.want {
				t.Errorf("isBreached(op=%q, value=%v, threshold=%v) = %v, want %v",
					tc.operator, tc.value, threshold, got, tc.want)
			}
		})
	}
}

// TestEvaluate_OKToFiring verifies that a breached snapshot causes the state
// to transition from OK to Firing and that the webhook is called exactly once.
func TestEvaluate_OKToFiring(t *testing.T) {
	rule := config.AlertRuleConfig{
		Name:      "high-failures",
		Condition: "connection_failures",
		Operator:  ">",
		Threshold: 2,
		Severity:  "critical",
	}
	// ConsecutiveFailures = 5 > 2, so the rule is breached.
	snap := metrics.Snapshot{ConsecutiveFailures: 5}

	poster := &mockHTTPPoster{responses: []mockPostResponse{newOKResponse()}}
	engine := singleRuleEngine(rule, snap, poster)

	// Confirm initial state is OK.
	if engine.ruleStates[rule.Name].State != StateOK {
		t.Fatal("expected initial state to be StateOK")
	}

	engine.evaluate()

	if engine.ruleStates[rule.Name].State != StateFiring {
		t.Errorf("expected state StateFiring after breached evaluation, got %v",
			engine.ruleStates[rule.Name].State)
	}
	if poster.callCount() != 1 {
		t.Errorf("expected 1 webhook call, got %d", poster.callCount())
	}
}

// TestEvaluate_FiringToOK verifies that when a previously firing rule is no
// longer breached the state transitions to OK and a resolved card is sent.
func TestEvaluate_FiringToOK(t *testing.T) {
	rule := config.AlertRuleConfig{
		Name:      "high-failures",
		Condition: "connection_failures",
		Operator:  ">",
		Threshold: 2,
		Severity:  "warning",
	}
	// Healthy snapshot – failures = 0, not breached.
	snap := metrics.Snapshot{ConsecutiveFailures: 0}

	poster := &mockHTTPPoster{responses: []mockPostResponse{newOKResponse()}}
	engine := singleRuleEngine(rule, snap, poster)

	// Force the state to Firing so we can test the resolved transition.
	engine.ruleStates[rule.Name].State = StateFiring
	engine.ruleStates[rule.Name].FiredAt = time.Now().Add(-5 * time.Minute)

	engine.evaluate()

	if engine.ruleStates[rule.Name].State != StateOK {
		t.Errorf("expected state StateOK after healthy evaluation, got %v",
			engine.ruleStates[rule.Name].State)
	}
	if poster.callCount() != 1 {
		t.Errorf("expected 1 resolved-card webhook call, got %d", poster.callCount())
	}
}

// TestEvaluate_StaysInFiring verifies that no additional webhook call is made
// when the rule is already firing and the snapshot is still breached.
func TestEvaluate_StaysInFiring(t *testing.T) {
	rule := config.AlertRuleConfig{
		Name:      "high-failures",
		Condition: "connection_failures",
		Operator:  ">",
		Threshold: 2,
		Severity:  "warning",
	}
	snap := metrics.Snapshot{ConsecutiveFailures: 5}

	poster := &mockHTTPPoster{responses: []mockPostResponse{newOKResponse()}}
	engine := singleRuleEngine(rule, snap, poster)

	// Pre-set state to Firing.
	engine.ruleStates[rule.Name].State = StateFiring
	engine.ruleStates[rule.Name].FiredAt = time.Now().Add(-2 * time.Minute)

	engine.evaluate()

	if engine.ruleStates[rule.Name].State != StateFiring {
		t.Errorf("expected state to remain StateFiring, got %v",
			engine.ruleStates[rule.Name].State)
	}
	if poster.callCount() != 0 {
		t.Errorf("expected 0 webhook calls when staying in Firing, got %d", poster.callCount())
	}
}

// TestEvaluate_StaysInOK verifies that no webhook call is made when the rule
// is already OK and the snapshot is not breached.
func TestEvaluate_StaysInOK(t *testing.T) {
	rule := config.AlertRuleConfig{
		Name:      "high-failures",
		Condition: "connection_failures",
		Operator:  ">",
		Threshold: 2,
		Severity:  "warning",
	}
	snap := metrics.Snapshot{ConsecutiveFailures: 0}

	poster := &mockHTTPPoster{responses: []mockPostResponse{newOKResponse()}}
	engine := singleRuleEngine(rule, snap, poster)

	engine.evaluate()

	if engine.ruleStates[rule.Name].State != StateOK {
		t.Errorf("expected state to remain StateOK, got %v",
			engine.ruleStates[rule.Name].State)
	}
	if poster.callCount() != 0 {
		t.Errorf("expected 0 webhook calls when staying in OK, got %d", poster.callCount())
	}
}

// TestSendAlertCard_JSON captures the JSON payload produced by sendAlertCard
// and validates the Adaptive Card structure.
func TestSendAlertCard_JSON(t *testing.T) {
	rule := config.AlertRuleConfig{
		Name:      "latency-spike",
		Condition: "round_trip_p95_ms",
		Operator:  ">",
		Threshold: 500,
		Severity:  "critical",
	}

	poster := &mockHTTPPoster{responses: []mockPostResponse{newOKResponse()}}
	engine := singleRuleEngine(rule, metrics.Snapshot{}, poster)

	engine.sendAlertCard(rule, 612.34, true)

	if poster.callCount() != 1 {
		t.Fatalf("expected 1 POST call, got %d", poster.callCount())
	}

	body := poster.lastBody()
	if len(body) == 0 {
		t.Fatal("expected non-empty POST body")
	}

	// Unmarshal into a generic structure to validate the card shape.
	var card map[string]any
	if err := json.Unmarshal(body, &card); err != nil {
		t.Fatalf("POST body is not valid JSON: %v\nbody: %s", err, body)
	}

	// Top-level "type" must be "message".
	if card["type"] != "message" {
		t.Errorf("card.type = %v, want \"message\"", card["type"])
	}

	// Must have exactly one attachment.
	attachments, ok := card["attachments"].([]any)
	if !ok || len(attachments) == 0 {
		t.Fatal("expected non-empty attachments array")
	}

	att, ok := attachments[0].(map[string]any)
	if !ok {
		t.Fatal("attachment is not an object")
	}
	if att["contentType"] != "application/vnd.microsoft.card.adaptive" {
		t.Errorf("attachment.contentType = %v, want adaptive card type", att["contentType"])
	}

	content, ok := att["content"].(map[string]any)
	if !ok {
		t.Fatal("attachment.content is not an object")
	}
	if content["type"] != "AdaptiveCard" {
		t.Errorf("content.type = %v, want \"AdaptiveCard\"", content["type"])
	}
	if content["version"] != "1.4" {
		t.Errorf("content.version = %v, want \"1.4\"", content["version"])
	}

	// Body must contain a TextBlock and a FactSet.
	bodyArr, ok := content["body"].([]any)
	if !ok || len(bodyArr) < 2 {
		t.Fatalf("expected at least 2 body elements, got %v", content["body"])
	}

	textBlock, ok := bodyArr[0].(map[string]any)
	if !ok {
		t.Fatal("body[0] is not an object")
	}
	if textBlock["type"] != "TextBlock" {
		t.Errorf("body[0].type = %v, want \"TextBlock\"", textBlock["type"])
	}
	titleStr, _ := textBlock["text"].(string)
	if !strings.Contains(titleStr, "latency-spike") {
		t.Errorf("TextBlock title %q does not contain rule name", titleStr)
	}

	factSet, ok := bodyArr[1].(map[string]any)
	if !ok {
		t.Fatal("body[1] is not an object")
	}
	if factSet["type"] != "FactSet" {
		t.Errorf("body[1].type = %v, want \"FactSet\"", factSet["type"])
	}

	facts, ok := factSet["facts"].([]any)
	if !ok || len(facts) == 0 {
		t.Fatal("FactSet.facts is empty or missing")
	}

	// Verify the "Current Value" fact contains the formatted value.
	foundValue := false
	for _, f := range facts {
		fact, ok := f.(map[string]any)
		if !ok {
			continue
		}
		if fact["title"] == "Current Value" {
			foundValue = true
			if !strings.Contains(fact["value"].(string), "612.34") {
				t.Errorf("Current Value fact = %v, expected to contain \"612.34\"", fact["value"])
			}
		}
	}
	if !foundValue {
		t.Error("FactSet does not contain a \"Current Value\" fact")
	}
}

// TestSendHourlyReport verifies that sendHourlyReport posts a card whose
// TextBlock title mentions "Report" and whose FactSet includes uptime and
// round-trip facts.
func TestSendHourlyReport(t *testing.T) {
	snap := metrics.Snapshot{
		Connected:           true,
		UptimePercent:       97.5,
		RoundTripAvgMs:      18.2,
		RoundTripP95Ms:      45.0,
		RoundTripMaxMs:      102.0,
		MessagesSent:        120,
		MessagesReceived:    118,
		DeliverySuccessRate: 0.983,
		ReconnectCount:      1,
	}

	poster := &mockHTTPPoster{responses: []mockPostResponse{newOKResponse()}}
	cfg := config.AlertsConfig{
		TeamsWebhookURL: "https://example.webhook/post",
	}
	buf := &mockMetricsReader{snapshot: snap}
	engine := NewEngineWithPoster(cfg, "broker.example.com", buf, discardLogger(), poster)

	engine.sendHourlyReport()

	if poster.callCount() != 1 {
		t.Fatalf("expected 1 POST call, got %d", poster.callCount())
	}

	var card map[string]any
	if err := json.Unmarshal(poster.lastBody(), &card); err != nil {
		t.Fatalf("response body is not valid JSON: %v", err)
	}

	attachments := card["attachments"].([]any)
	att := attachments[0].(map[string]any)
	content := att["content"].(map[string]any)
	bodyArr := content["body"].([]any)

	textBlock := bodyArr[0].(map[string]any)
	if !strings.Contains(textBlock["text"].(string), "Report") {
		t.Errorf("report title %q does not contain \"Report\"", textBlock["text"])
	}

	factSet := bodyArr[1].(map[string]any)
	facts := factSet["facts"].([]any)

	wantTitles := []string{"Uptime", "Round-trip Avg", "Round-trip P95", "Messages Sent"}
	for _, want := range wantTitles {
		found := false
		for _, f := range facts {
			fact := f.(map[string]any)
			if strings.Contains(fact["title"].(string), want) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("hourly report facts missing entry with title containing %q", want)
		}
	}
}

// TestPostToTeams_Retry429 verifies that a 429 response causes postToTeams to
// retry and that all three attempts are made before succeeding on the third.
func TestPostToTeams_Retry429(t *testing.T) {
	// Suppress sleep delays by running in a test that accepts the timing overhead,
	// or we simply keep retries minimal (max 3) and let the test run.
	// The production code sleeps 2s then 4s; to keep the test fast we accept
	// that this test may take ~6s. If that is unacceptable, the sleep logic in
	// postToTeams would need to be injectable. For now correctness over speed.
	if testing.Short() {
		t.Skip("skipping retry test in short mode (involves sleep)")
	}

	poster := &mockHTTPPoster{
		responses: []mockPostResponse{
			new429Response(),
			new429Response(),
			newOKResponse(), // succeeds on third attempt
		},
	}

	cfg := config.AlertsConfig{TeamsWebhookURL: "https://example.webhook/post"}
	engine := NewEngineWithPoster(cfg, "broker.example.com", &mockMetricsReader{}, discardLogger(), poster)

	card := engine.buildAdaptiveCard("test", "default", []Fact{{Title: "k", Value: "v"}})
	engine.postToTeams(card)

	if poster.callCount() != 3 {
		t.Errorf("expected 3 POST attempts (2x429 + 1x200), got %d", poster.callCount())
	}
}

// TestPostToTeams_PermanentFailure verifies that three consecutive 500
// responses are retried exhaustively and an error is logged (no panic).
func TestPostToTeams_PermanentFailure(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping retry test in short mode (involves sleep)")
	}

	poster := &mockHTTPPoster{
		responses: []mockPostResponse{
			new500Response(),
			new500Response(),
			new500Response(),
		},
	}

	// Route logs to a buffer so we can assert an error was logged.
	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	cfg := config.AlertsConfig{TeamsWebhookURL: "https://example.webhook/post"}
	engine := NewEngineWithPoster(cfg, "broker.example.com", &mockMetricsReader{}, logger, poster)

	card := engine.buildAdaptiveCard("test", "default", []Fact{{Title: "k", Value: "v"}})
	engine.postToTeams(card)

	if poster.callCount() != 3 {
		t.Errorf("expected exactly 3 POST attempts, got %d", poster.callCount())
	}

	logOutput := logBuf.String()
	if !strings.Contains(logOutput, "failed to send teams notification") {
		t.Errorf("expected error log message not found in output:\n%s", logOutput)
	}
}

// TestActiveAlerts verifies that only firing rules are returned and that the
// returned values are copies (modifying them does not affect engine state).
func TestActiveAlerts(t *testing.T) {
	rules := []config.AlertRuleConfig{
		{Name: "rule-firing-1", Condition: "connection_failures", Operator: ">", Threshold: 1},
		{Name: "rule-ok", Condition: "uptime_percent", Operator: "<", Threshold: 50},
		{Name: "rule-firing-2", Condition: "reconnect_count", Operator: ">", Threshold: 0},
	}
	cfg := config.AlertsConfig{
		TeamsWebhookURL: "https://example.webhook/post",
		Rules:           rules,
	}
	engine := NewEngineWithPoster(cfg, "broker.example.com", &mockMetricsReader{}, discardLogger(),
		&mockHTTPPoster{responses: []mockPostResponse{newOKResponse()}})

	firingTime := time.Now().Add(-10 * time.Minute)

	engine.ruleStates["rule-firing-1"].State = StateFiring
	engine.ruleStates["rule-firing-1"].FiredAt = firingTime
	engine.ruleStates["rule-firing-1"].LastValue = 5.0

	// rule-ok remains StateOK (default from NewEngineWithPoster).

	engine.ruleStates["rule-firing-2"].State = StateFiring
	engine.ruleStates["rule-firing-2"].FiredAt = firingTime
	engine.ruleStates["rule-firing-2"].LastValue = 3.0

	active := engine.ActiveAlerts()

	if len(active) != 2 {
		t.Fatalf("expected 2 active alerts, got %d: %v", len(active), active)
	}
	if _, ok := active["rule-firing-1"]; !ok {
		t.Error("rule-firing-1 should be in active alerts")
	}
	if _, ok := active["rule-firing-2"]; !ok {
		t.Error("rule-firing-2 should be in active alerts")
	}
	if _, ok := active["rule-ok"]; ok {
		t.Error("rule-ok should not be in active alerts")
	}

	// Mutate the returned copy – engine state must be unaffected.
	copy1 := active["rule-firing-1"]
	copy1.State = StateOK
	copy1.LastValue = 999
	if engine.ruleStates["rule-firing-1"].State != StateFiring {
		t.Error("mutating returned copy should not affect engine state")
	}
	if engine.ruleStates["rule-firing-1"].LastValue == 999 {
		t.Error("mutating returned copy should not affect LastValue in engine state")
	}
}

// TestFormatDuration verifies the human-readable output for various durations.
func TestFormatDuration(t *testing.T) {
	cases := []struct {
		duration time.Duration
		want     string
	}{
		// Seconds-only range (< 1 minute)
		{0 * time.Second, "0s"},
		{1 * time.Second, "1s"},
		{59 * time.Second, "59s"},

		// Minutes range (>= 1 minute, < 1 hour)
		{1 * time.Minute, "1m 0s"},
		{1*time.Minute + 30*time.Second, "1m 30s"},
		{59*time.Minute + 59*time.Second, "59m 59s"},

		// Hours range (>= 1 hour)
		{1 * time.Hour, "1h 0m"},
		{2*time.Hour + 15*time.Minute, "2h 15m"},
		{25*time.Hour + 3*time.Minute, "25h 3m"},
	}

	for _, tc := range cases {
		t.Run(tc.want, func(t *testing.T) {
			got := formatDuration(tc.duration)
			if got != tc.want {
				t.Errorf("formatDuration(%v) = %q, want %q", tc.duration, got, tc.want)
			}
		})
	}
}

// TestMain ensures the test binary has access to stderr for slog output.
func TestMain(m *testing.M) {
	// Replace the default logger so package-level slog calls don't interfere.
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	os.Exit(m.Run())
}
