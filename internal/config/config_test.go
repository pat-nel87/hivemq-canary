package config

import (
	"os"
	"strings"
	"testing"
	"time"
)

// writeTemp writes content to a new temporary file and returns the path.
// The caller is responsible for removing the file via t.Cleanup.
func writeTemp(t *testing.T, content string) string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "hivemq-canary-config-*.yaml")
	if err != nil {
		t.Fatalf("creating temp config file: %v", err)
	}
	if _, err := f.WriteString(content); err != nil {
		t.Fatalf("writing temp config file: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("closing temp config file: %v", err)
	}
	return f.Name()
}

// minimalValid returns the smallest YAML that satisfies all required fields.
func minimalValid() string {
	return `
broker:
  host: broker.example.com
  username: testuser
  password: testpass
alerts:
  teams_webhook_url: https://teams.example.com/webhook
`
}

func TestLoad_ValidConfig(t *testing.T) {
	yaml := `
broker:
  host: broker.example.com
  port: 8884
  tls: true
  username: alice
  password: s3cr3t
  client_id: my-canary

canary:
  interval: 60s
  timeout: 20s
  topic_prefix: custom/prefix
  qos_levels: [0, 1, 2]

alerts:
  teams_webhook_url: https://teams.example.com/webhook
  hourly_report:
    enabled: true
    interval: 2h
    include_when_healthy: true
  rules:
    - name: high-latency
      condition: latency_ms
      threshold: 500.0
      operator: ">"
      severity: critical
      window: 5m

storage:
  azure_blob:
    enabled: true
    container: canary-data
    account_name: mystorageacct
    account_key: base64key==
    retention_days: 30

server:
  port: 9090
`
	path := writeTemp(t, yaml)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() unexpected error: %v", err)
	}

	t.Run("broker fields", func(t *testing.T) {
		if cfg.Broker.Host != "broker.example.com" {
			t.Errorf("Broker.Host = %q, want %q", cfg.Broker.Host, "broker.example.com")
		}
		if cfg.Broker.Port != 8884 {
			t.Errorf("Broker.Port = %d, want 8884", cfg.Broker.Port)
		}
		if !cfg.Broker.TLS {
			t.Error("Broker.TLS = false, want true")
		}
		if cfg.Broker.Username != "alice" {
			t.Errorf("Broker.Username = %q, want %q", cfg.Broker.Username, "alice")
		}
		if cfg.Broker.Password != "s3cr3t" {
			t.Errorf("Broker.Password = %q, want %q", cfg.Broker.Password, "s3cr3t")
		}
		if cfg.Broker.ClientID != "my-canary" {
			t.Errorf("Broker.ClientID = %q, want %q", cfg.Broker.ClientID, "my-canary")
		}
	})

	t.Run("canary fields", func(t *testing.T) {
		if cfg.Canary.Interval != 60*time.Second {
			t.Errorf("Canary.Interval = %v, want 60s", cfg.Canary.Interval)
		}
		if cfg.Canary.Timeout != 20*time.Second {
			t.Errorf("Canary.Timeout = %v, want 20s", cfg.Canary.Timeout)
		}
		if cfg.Canary.TopicPrefix != "custom/prefix" {
			t.Errorf("Canary.TopicPrefix = %q, want %q", cfg.Canary.TopicPrefix, "custom/prefix")
		}
		wantQoS := []int{0, 1, 2}
		if len(cfg.Canary.QoSLevels) != len(wantQoS) {
			t.Errorf("Canary.QoSLevels = %v, want %v", cfg.Canary.QoSLevels, wantQoS)
		} else {
			for i, v := range wantQoS {
				if cfg.Canary.QoSLevels[i] != v {
					t.Errorf("Canary.QoSLevels[%d] = %d, want %d", i, cfg.Canary.QoSLevels[i], v)
				}
			}
		}
	})

	t.Run("alerts fields", func(t *testing.T) {
		if cfg.Alerts.TeamsWebhookURL != "https://teams.example.com/webhook" {
			t.Errorf("Alerts.TeamsWebhookURL = %q", cfg.Alerts.TeamsWebhookURL)
		}
		if !cfg.Alerts.HourlyReport.Enabled {
			t.Error("HourlyReport.Enabled = false, want true")
		}
		if cfg.Alerts.HourlyReport.Interval != 2*time.Hour {
			t.Errorf("HourlyReport.Interval = %v, want 2h", cfg.Alerts.HourlyReport.Interval)
		}
		if !cfg.Alerts.HourlyReport.IncludeWhenHealthy {
			t.Error("HourlyReport.IncludeWhenHealthy = false, want true")
		}
		if len(cfg.Alerts.Rules) != 1 {
			t.Fatalf("len(Alerts.Rules) = %d, want 1", len(cfg.Alerts.Rules))
		}
		rule := cfg.Alerts.Rules[0]
		if rule.Name != "high-latency" {
			t.Errorf("Rules[0].Name = %q, want %q", rule.Name, "high-latency")
		}
		if rule.Condition != "latency_ms" {
			t.Errorf("Rules[0].Condition = %q, want %q", rule.Condition, "latency_ms")
		}
		if rule.Threshold != 500.0 {
			t.Errorf("Rules[0].Threshold = %v, want 500.0", rule.Threshold)
		}
		if rule.Operator != ">" {
			t.Errorf("Rules[0].Operator = %q, want %q", rule.Operator, ">")
		}
		if rule.Severity != "critical" {
			t.Errorf("Rules[0].Severity = %q, want %q", rule.Severity, "critical")
		}
		if rule.Window != 5*time.Minute {
			t.Errorf("Rules[0].Window = %v, want 5m", rule.Window)
		}
	})

	t.Run("storage fields", func(t *testing.T) {
		ab := cfg.Storage.AzureBlob
		if !ab.Enabled {
			t.Error("AzureBlob.Enabled = false, want true")
		}
		if ab.Container != "canary-data" {
			t.Errorf("AzureBlob.Container = %q, want %q", ab.Container, "canary-data")
		}
		if ab.AccountName != "mystorageacct" {
			t.Errorf("AzureBlob.AccountName = %q, want %q", ab.AccountName, "mystorageacct")
		}
		if ab.AccountKey != "base64key==" {
			t.Errorf("AzureBlob.AccountKey = %q, want %q", ab.AccountKey, "base64key==")
		}
		if ab.RetentionDays != 30 {
			t.Errorf("AzureBlob.RetentionDays = %d, want 30", ab.RetentionDays)
		}
	})

	t.Run("server fields", func(t *testing.T) {
		if cfg.Server.Port != 9090 {
			t.Errorf("Server.Port = %d, want 9090", cfg.Server.Port)
		}
	})
}

func TestLoad_Defaults(t *testing.T) {
	path := writeTemp(t, minimalValid())
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() unexpected error: %v", err)
	}

	t.Run("broker port default", func(t *testing.T) {
		if cfg.Broker.Port != 8883 {
			t.Errorf("Broker.Port = %d, want 8883", cfg.Broker.Port)
		}
	})

	t.Run("broker client_id auto-generated", func(t *testing.T) {
		if cfg.Broker.ClientID == "" {
			t.Error("Broker.ClientID is empty, want auto-generated value")
		}
		if !strings.HasPrefix(cfg.Broker.ClientID, "hivemq-canary-") {
			t.Errorf("Broker.ClientID = %q, want prefix %q", cfg.Broker.ClientID, "hivemq-canary-")
		}
	})

	t.Run("canary interval default", func(t *testing.T) {
		if cfg.Canary.Interval != 30*time.Second {
			t.Errorf("Canary.Interval = %v, want 30s", cfg.Canary.Interval)
		}
	})

	t.Run("canary timeout default", func(t *testing.T) {
		if cfg.Canary.Timeout != 10*time.Second {
			t.Errorf("Canary.Timeout = %v, want 10s", cfg.Canary.Timeout)
		}
	})

	t.Run("canary topic_prefix default", func(t *testing.T) {
		if cfg.Canary.TopicPrefix != "monitor/canary" {
			t.Errorf("Canary.TopicPrefix = %q, want %q", cfg.Canary.TopicPrefix, "monitor/canary")
		}
	})

	t.Run("canary qos_levels default", func(t *testing.T) {
		want := []int{0, 1}
		if len(cfg.Canary.QoSLevels) != len(want) {
			t.Errorf("Canary.QoSLevels = %v, want %v", cfg.Canary.QoSLevels, want)
		} else {
			for i, v := range want {
				if cfg.Canary.QoSLevels[i] != v {
					t.Errorf("Canary.QoSLevels[%d] = %d, want %d", i, cfg.Canary.QoSLevels[i], v)
				}
			}
		}
	})

	t.Run("server port default", func(t *testing.T) {
		if cfg.Server.Port != 8080 {
			t.Errorf("Server.Port = %d, want 8080", cfg.Server.Port)
		}
	})

	t.Run("storage retention_days default", func(t *testing.T) {
		if cfg.Storage.AzureBlob.RetentionDays != 90 {
			t.Errorf("AzureBlob.RetentionDays = %d, want 90", cfg.Storage.AzureBlob.RetentionDays)
		}
	})

	t.Run("hourly_report enabled default", func(t *testing.T) {
		if !cfg.Alerts.HourlyReport.Enabled {
			t.Error("HourlyReport.Enabled = false, want true (default)")
		}
	})

	t.Run("hourly_report interval default", func(t *testing.T) {
		if cfg.Alerts.HourlyReport.Interval != 1*time.Hour {
			t.Errorf("HourlyReport.Interval = %v, want 1h", cfg.Alerts.HourlyReport.Interval)
		}
	})

	t.Run("hourly_report include_when_healthy default", func(t *testing.T) {
		if !cfg.Alerts.HourlyReport.IncludeWhenHealthy {
			t.Error("HourlyReport.IncludeWhenHealthy = false, want true (default)")
		}
	})
}

func TestLoad_EnvVarExpansion(t *testing.T) {
	t.Setenv("CANARY_HOST", "mqtt.example.com")
	t.Setenv("CANARY_USER", "envuser")
	t.Setenv("CANARY_PASS", "envpass")
	t.Setenv("CANARY_WEBHOOK", "https://teams.example.com/env-hook")

	yaml := `
broker:
  host: ${CANARY_HOST}
  username: ${CANARY_USER}
  password: ${CANARY_PASS}
alerts:
  teams_webhook_url: ${CANARY_WEBHOOK}
`
	path := writeTemp(t, yaml)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() unexpected error: %v", err)
	}

	if cfg.Broker.Host != "mqtt.example.com" {
		t.Errorf("Broker.Host = %q, want %q", cfg.Broker.Host, "mqtt.example.com")
	}
	if cfg.Broker.Username != "envuser" {
		t.Errorf("Broker.Username = %q, want %q", cfg.Broker.Username, "envuser")
	}
	if cfg.Broker.Password != "envpass" {
		t.Errorf("Broker.Password = %q, want %q", cfg.Broker.Password, "envpass")
	}
	if cfg.Alerts.TeamsWebhookURL != "https://teams.example.com/env-hook" {
		t.Errorf("Alerts.TeamsWebhookURL = %q, want %q", cfg.Alerts.TeamsWebhookURL, "https://teams.example.com/env-hook")
	}
}

func TestLoad_EnvVarNotSet(t *testing.T) {
	// Guarantee the variable is absent for this test.
	t.Setenv("CANARY_UNSET_VAR", "")
	os.Unsetenv("CANARY_UNSET_VAR") //nolint:errcheck

	yaml := `
broker:
  host: broker.example.com
  username: user
  password: pass
  client_id: ${CANARY_UNSET_VAR}
alerts:
  teams_webhook_url: https://teams.example.com/webhook
`
	path := writeTemp(t, yaml)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() unexpected error: %v", err)
	}

	// The literal "${CANARY_UNSET_VAR}" should be preserved, not collapsed to "".
	if cfg.Broker.ClientID != "${CANARY_UNSET_VAR}" {
		t.Errorf("Broker.ClientID = %q, want literal %q", cfg.Broker.ClientID, "${CANARY_UNSET_VAR}")
	}
}

func TestLoad_ValidationErrors(t *testing.T) {
	tests := []struct {
		name        string
		yaml        string
		wantMissing []string
	}{
		{
			name: "all required fields absent",
			yaml: `
canary:
  interval: 10s
`,
			wantMissing: []string{
				"broker.host is required",
				"broker.username is required",
				"broker.password is required",
				"alerts.teams_webhook_url is required",
			},
		},
		{
			name: "missing broker.host only",
			yaml: `
broker:
  username: u
  password: p
alerts:
  teams_webhook_url: https://teams.example.com/webhook
`,
			wantMissing: []string{"broker.host is required"},
		},
		{
			name: "missing broker.username only",
			yaml: `
broker:
  host: broker.example.com
  password: p
alerts:
  teams_webhook_url: https://teams.example.com/webhook
`,
			wantMissing: []string{"broker.username is required"},
		},
		{
			name: "missing broker.password only",
			yaml: `
broker:
  host: broker.example.com
  username: u
alerts:
  teams_webhook_url: https://teams.example.com/webhook
`,
			wantMissing: []string{"broker.password is required"},
		},
		{
			name: "missing teams_webhook_url only",
			yaml: `
broker:
  host: broker.example.com
  username: u
  password: p
`,
			wantMissing: []string{"alerts.teams_webhook_url is required"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := writeTemp(t, tt.yaml)
			_, err := Load(path)
			if err == nil {
				t.Fatal("Load() expected validation error, got nil")
			}
			for _, want := range tt.wantMissing {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not contain %q", err.Error(), want)
				}
			}
		})
	}
}

func TestLoad_RuleValidation(t *testing.T) {
	tests := []struct {
		name      string
		yaml      string
		wantInErr string
	}{
		{
			name: "rule missing name",
			yaml: `
broker:
  host: broker.example.com
  username: u
  password: p
alerts:
  teams_webhook_url: https://teams.example.com/webhook
  rules:
    - condition: latency_ms
      threshold: 200
`,
			wantInErr: "alerts.rules[0].name is required",
		},
		{
			name: "rule missing condition",
			yaml: `
broker:
  host: broker.example.com
  username: u
  password: p
alerts:
  teams_webhook_url: https://teams.example.com/webhook
  rules:
    - name: my-rule
      threshold: 200
`,
			wantInErr: "alerts.rules[0].condition is required",
		},
		{
			name: "rule missing both name and condition",
			yaml: `
broker:
  host: broker.example.com
  username: u
  password: p
alerts:
  teams_webhook_url: https://teams.example.com/webhook
  rules:
    - threshold: 200
`,
			wantInErr: "alerts.rules[0].name is required",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := writeTemp(t, tt.yaml)
			_, err := Load(path)
			if err == nil {
				t.Fatal("Load() expected validation error, got nil")
			}
			if !strings.Contains(err.Error(), tt.wantInErr) {
				t.Errorf("error %q does not contain %q", err.Error(), tt.wantInErr)
			}
		})
	}
}

func TestLoad_HourlyReportDisabled(t *testing.T) {
	// Phase 1 fix: an explicit "enabled: false" must survive default application.
	// setDefaults must not unconditionally overwrite the field back to true.
	yaml := `
broker:
  host: broker.example.com
  username: u
  password: p
alerts:
  teams_webhook_url: https://teams.example.com/webhook
  hourly_report:
    enabled: false
`
	path := writeTemp(t, yaml)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() unexpected error: %v", err)
	}

	if cfg.Alerts.HourlyReport.Enabled {
		t.Error("HourlyReport.Enabled = true, want false (explicit config must not be overwritten by defaults)")
	}
}

func TestLoad_IncludeWhenHealthyFalse(t *testing.T) {
	yaml := `
broker:
  host: broker.example.com
  username: u
  password: p
alerts:
  teams_webhook_url: https://teams.example.com/webhook
  hourly_report:
    enabled: true
    include_when_healthy: false
`
	path := writeTemp(t, yaml)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() unexpected error: %v", err)
	}

	if cfg.Alerts.HourlyReport.IncludeWhenHealthy {
		t.Error("HourlyReport.IncludeWhenHealthy = true, want false (explicit false must not be overwritten by defaults)")
	}
}

func TestLoad_RuleDefaults(t *testing.T) {
	yaml := `
broker:
  host: broker.example.com
  username: u
  password: p
alerts:
  teams_webhook_url: https://teams.example.com/webhook
  rules:
    - name: latency-check
      condition: latency_ms
      threshold: 300
`
	path := writeTemp(t, yaml)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() unexpected error: %v", err)
	}

	if len(cfg.Alerts.Rules) != 1 {
		t.Fatalf("len(Alerts.Rules) = %d, want 1", len(cfg.Alerts.Rules))
	}
	rule := cfg.Alerts.Rules[0]

	t.Run("default operator", func(t *testing.T) {
		if rule.Operator != ">" {
			t.Errorf("Rules[0].Operator = %q, want %q", rule.Operator, ">")
		}
	})

	t.Run("default severity", func(t *testing.T) {
		if rule.Severity != "warning" {
			t.Errorf("Rules[0].Severity = %q, want %q", rule.Severity, "warning")
		}
	})
}

func TestLoad_FileNotFound(t *testing.T) {
	_, err := Load("/nonexistent/path/to/config.yaml")
	if err == nil {
		t.Fatal("Load() expected error for missing file, got nil")
	}
	// The error must mention the file-read step so the caller can diagnose it.
	if !strings.Contains(err.Error(), "reading config file") {
		t.Errorf("error %q does not contain %q", err.Error(), "reading config file")
	}
}
