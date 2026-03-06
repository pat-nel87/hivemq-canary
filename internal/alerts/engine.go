package alerts

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"hivemq-canary/internal/config"
	"hivemq-canary/internal/metrics"
)

// HTTPPoster abstracts HTTP POST operations for testability.
type HTTPPoster interface {
	Post(url, contentType string, body io.Reader) (*http.Response, error)
}

// AlertState tracks the lifecycle of a single alert rule.
type AlertState int

const (
	StateOK AlertState = iota
	StateFiring
)

// RuleState holds the runtime state for one alert rule.
type RuleState struct {
	State     AlertState
	FiredAt   time.Time
	ResolvedAt time.Time
	LastValue float64
}

// Engine evaluates alert rules and sends Teams notifications.
type Engine struct {
	cfg        config.AlertsConfig
	brokerHost string
	buffer     metrics.MetricsReader
	logger     *slog.Logger
	httpClient HTTPPoster

	mu         sync.Mutex
	ruleStates map[string]*RuleState // keyed by rule name
}

// NewEngine creates the alert engine.
func NewEngine(cfg config.AlertsConfig, brokerHost string, buffer metrics.MetricsReader, logger *slog.Logger) *Engine {
	return NewEngineWithPoster(cfg, brokerHost, buffer, logger, &http.Client{Timeout: 15 * time.Second})
}

// NewEngineWithPoster creates the alert engine with a custom HTTP poster (for testing).
func NewEngineWithPoster(cfg config.AlertsConfig, brokerHost string, buffer metrics.MetricsReader, logger *slog.Logger, poster HTTPPoster) *Engine {
	states := make(map[string]*RuleState)
	for _, rule := range cfg.Rules {
		states[rule.Name] = &RuleState{State: StateOK}
	}

	return &Engine{
		cfg:        cfg,
		brokerHost: brokerHost,
		buffer:     buffer,
		logger:     logger,
		httpClient: poster,
		ruleStates: states,
	}
}

// Start launches the alert evaluation loop and hourly report scheduler.
func (e *Engine) Start(ctx context.Context) {
	// Alert evaluation runs shortly after each canary check
	evalTicker := time.NewTicker(35 * time.Second)
	defer evalTicker.Stop()

	// Hourly report
	var reportCh <-chan time.Time
	if e.cfg.HourlyReport.Enabled {
		reportTicker := time.NewTicker(e.cfg.HourlyReport.Interval)
		defer reportTicker.Stop()
		reportCh = reportTicker.C
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-evalTicker.C:
			e.evaluate()
		case <-reportCh:
			e.sendHourlyReport()
		}
	}
}

// ActiveAlerts returns copies of currently firing alert states.
func (e *Engine) ActiveAlerts() map[string]RuleState {
	e.mu.Lock()
	defer e.mu.Unlock()

	active := make(map[string]RuleState)
	for name, state := range e.ruleStates {
		if state.State == StateFiring {
			active[name] = *state
		}
	}
	return active
}

func (e *Engine) evaluate() {
	snap := e.buffer.SnapshotSince(time.Now().Add(-5 * time.Minute))

	for _, rule := range e.cfg.Rules {
		value := e.extractMetric(rule.Condition, &snap)
		breached := e.isBreached(rule, value)

		e.mu.Lock()
		state := e.ruleStates[rule.Name]

		switch {
		case breached && state.State == StateOK:
			// Transition: OK -> FIRING
			state.State = StateFiring
			state.FiredAt = time.Now()
			state.LastValue = value
			e.mu.Unlock()

			e.logger.Warn("alert firing",
				"rule", rule.Name,
				"value", value,
				"threshold", rule.Threshold,
			)
			e.sendAlertCard(rule, value, true)

		case !breached && state.State == StateFiring:
			// Transition: FIRING -> OK (resolved)
			duration := time.Since(state.FiredAt)
			state.State = StateOK
			state.ResolvedAt = time.Now()
			state.LastValue = value
			e.mu.Unlock()

			e.logger.Info("alert resolved",
				"rule", rule.Name,
				"duration", duration,
			)
			e.sendResolvedCard(rule, value, duration)

		default:
			state.LastValue = value
			e.mu.Unlock()
		}
	}
}

func (e *Engine) extractMetric(condition string, snap *metrics.Snapshot) float64 {
	switch condition {
	case "connection_failures":
		return float64(snap.ConsecutiveFailures)
	case "round_trip_p95_ms":
		return snap.RoundTripP95Ms
	case "round_trip_avg_ms":
		return snap.RoundTripAvgMs
	case "delivery_success_rate":
		return snap.DeliverySuccessRate
	case "reconnect_count":
		return float64(snap.ReconnectCount)
	case "uptime_percent":
		return snap.UptimePercent
	default:
		e.logger.Warn("unknown metric condition", "condition", condition)
		return 0
	}
}

func (e *Engine) isBreached(rule config.AlertRuleConfig, value float64) bool {
	switch rule.Operator {
	case ">":
		return value > rule.Threshold
	case ">=":
		return value >= rule.Threshold
	case "<":
		return value < rule.Threshold
	case "<=":
		return value <= rule.Threshold
	case "==":
		return value == rule.Threshold
	default:
		return value > rule.Threshold
	}
}

// --- Teams Adaptive Card Builders ---

func (e *Engine) sendAlertCard(rule config.AlertRuleConfig, value float64, firing bool) {
	color := "attention" // yellow
	icon := "⚠️"
	if rule.Severity == "critical" {
		color = "attention"
		icon = "🔴"
	}

	card := e.buildAdaptiveCard(
		fmt.Sprintf("%s ALERT: %s", icon, rule.Name),
		color,
		[]Fact{
			{Title: "Cluster", Value: e.brokerHost},
			{Title: "Severity", Value: rule.Severity},
			{Title: "Condition", Value: fmt.Sprintf("%s %s %.2f", rule.Condition, rule.Operator, rule.Threshold)},
			{Title: "Current Value", Value: fmt.Sprintf("%.2f", value)},
			{Title: "Fired At", Value: time.Now().UTC().Format(time.RFC3339)},
		},
	)

	e.postToTeams(card)
}

func (e *Engine) sendResolvedCard(rule config.AlertRuleConfig, value float64, duration time.Duration) {
	card := e.buildAdaptiveCard(
		fmt.Sprintf("🟢 RESOLVED: %s", rule.Name),
		"good",
		[]Fact{
			{Title: "Cluster", Value: e.brokerHost},
			{Title: "Condition", Value: fmt.Sprintf("%s %s %.2f", rule.Condition, rule.Operator, rule.Threshold)},
			{Title: "Current Value", Value: fmt.Sprintf("%.2f", value)},
			{Title: "Duration", Value: formatDuration(duration)},
			{Title: "Resolved At", Value: time.Now().UTC().Format(time.RFC3339)},
		},
	)

	e.postToTeams(card)
}

func (e *Engine) sendHourlyReport() {
	snap := e.buffer.SnapshotSince(time.Now().Add(-1 * time.Hour))

	statusIcon := "🟢"
	statusText := "Healthy"
	activeAlerts := e.ActiveAlerts()

	if len(activeAlerts) > 0 {
		statusIcon = "🔴"
		statusText = fmt.Sprintf("%d Active Alert(s)", len(activeAlerts))
	} else if snap.UptimePercent < 100 {
		statusIcon = "🟡"
		statusText = "Degraded"
	}

	facts := []Fact{
		{Title: "Status", Value: fmt.Sprintf("%s %s", statusIcon, statusText)},
		{Title: "Cluster", Value: e.brokerHost},
		{Title: "Connected", Value: fmt.Sprintf("%v", snap.Connected)},
		{Title: "Uptime", Value: fmt.Sprintf("%.1f%%", snap.UptimePercent)},
		{Title: "", Value: ""},
		{Title: "Round-trip Avg", Value: fmt.Sprintf("%.1f ms", snap.RoundTripAvgMs)},
		{Title: "Round-trip P95", Value: fmt.Sprintf("%.1f ms", snap.RoundTripP95Ms)},
		{Title: "Round-trip Max", Value: fmt.Sprintf("%.1f ms", snap.RoundTripMaxMs)},
		{Title: "", Value: ""},
		{Title: "Messages Sent", Value: fmt.Sprintf("%d", snap.MessagesSent)},
		{Title: "Messages Received", Value: fmt.Sprintf("%d", snap.MessagesReceived)},
		{Title: "Delivery Rate", Value: fmt.Sprintf("%.1f%%", snap.DeliverySuccessRate*100)},
		{Title: "Reconnects", Value: fmt.Sprintf("%d", snap.ReconnectCount)},
	}

	// Append active alerts
	if len(activeAlerts) > 0 {
		facts = append(facts, Fact{Title: "", Value: ""})
		facts = append(facts, Fact{Title: "Active Alerts", Value: ""})
		for name, state := range activeAlerts {
			duration := time.Since(state.FiredAt)
			facts = append(facts, Fact{
				Title: name,
				Value: fmt.Sprintf("firing for %s (value: %.2f)", formatDuration(duration), state.LastValue),
			})
		}
	}

	if snap.LastError != "" {
		facts = append(facts, Fact{Title: "", Value: ""})
		facts = append(facts, Fact{Title: "Last Error", Value: snap.LastError})
	}

	facts = append(facts, Fact{Title: "", Value: ""})
	facts = append(facts, Fact{Title: "Report Time", Value: time.Now().UTC().Format(time.RFC3339)})

	card := e.buildAdaptiveCard(
		"📊 HiveMQ Broker Health Report",
		"default",
		facts,
	)

	e.postToTeams(card)
}

// --- Adaptive Card JSON Structure ---

type Fact struct {
	Title string `json:"title"`
	Value string `json:"value"`
}

type adaptiveCard struct {
	Type        string       `json:"type"`
	Attachments []attachment `json:"attachments"`
}

type attachment struct {
	ContentType string      `json:"contentType"`
	Content     cardContent `json:"content"`
}

type cardContent struct {
	Schema  string        `json:"$schema"`
	Type    string        `json:"type"`
	Version string        `json:"version"`
	Body    []any `json:"body"`
}

type textBlock struct {
	Type   string `json:"type"`
	Text   string `json:"text"`
	Size   string `json:"size,omitempty"`
	Weight string `json:"weight,omitempty"`
	Color  string `json:"color,omitempty"`
	Wrap   bool   `json:"wrap,omitempty"`
}

type factSet struct {
	Type  string `json:"type"`
	Facts []Fact `json:"facts"`
}

func (e *Engine) buildAdaptiveCard(title, color string, facts []Fact) adaptiveCard {
	// Filter out empty separator facts for the FactSet
	cleanFacts := make([]Fact, 0, len(facts))
	for _, f := range facts {
		if f.Title != "" || f.Value != "" {
			cleanFacts = append(cleanFacts, f)
		}
	}

	body := []any{
		textBlock{
			Type:   "TextBlock",
			Text:   title,
			Size:   "Large",
			Weight: "Bolder",
			Wrap:   true,
		},
		factSet{
			Type:  "FactSet",
			Facts: cleanFacts,
		},
	}

	return adaptiveCard{
		Type: "message",
		Attachments: []attachment{
			{
				ContentType: "application/vnd.microsoft.card.adaptive",
				Content: cardContent{
					Schema:  "http://adaptivecards.io/schemas/adaptive-card.json",
					Type:    "AdaptiveCard",
					Version: "1.4",
					Body:    body,
				},
			},
		},
	}
}

func (e *Engine) postToTeams(card adaptiveCard) {
	body, err := json.Marshal(card)
	if err != nil {
		e.logger.Error("failed to marshal adaptive card", "error", err)
		return
	}

	// Retry with exponential backoff (Teams rate limits at 4 req/s)
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			time.Sleep(time.Duration(attempt*2) * time.Second)
		}

		resp, err := e.httpClient.Post(e.cfg.TeamsWebhookURL, "application/json", bytes.NewReader(body))
		if err != nil {
			lastErr = err
			continue
		}
		resp.Body.Close()

		if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusAccepted {
			e.logger.Debug("teams notification sent")
			return
		}

		if resp.StatusCode == http.StatusTooManyRequests {
			e.logger.Warn("teams rate limited, retrying", "attempt", attempt+1)
			lastErr = fmt.Errorf("rate limited (429)")
			continue
		}

		lastErr = fmt.Errorf("teams webhook returned %d", resp.StatusCode)
	}

	e.logger.Error("failed to send teams notification", "error", lastErr)
}

func formatDuration(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm %ds", int(d.Minutes()), int(d.Seconds())%60)
	}
	return fmt.Sprintf("%dh %dm", int(d.Hours()), int(d.Minutes())%60)
}
