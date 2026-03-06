package config

import (
	"fmt"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the root configuration for hivemq-canary.
type Config struct {
	Broker  BrokerConfig  `yaml:"broker"`
	Canary  CanaryConfig  `yaml:"canary"`
	Alerts  AlertsConfig  `yaml:"alerts"`
	Storage StorageConfig `yaml:"storage"`
	Server  ServerConfig  `yaml:"server"`
}

// BrokerConfig holds MQTT connection details for the HiveMQ Cloud cluster.
type BrokerConfig struct {
	Host     string `yaml:"host"`
	Port     int    `yaml:"port"`
	TLS      bool   `yaml:"tls"`
	Username string `yaml:"username"`
	Password string `yaml:"password"`
	ClientID string `yaml:"client_id"`
}

// CanaryConfig controls the canary check behavior.
type CanaryConfig struct {
	Interval    time.Duration `yaml:"interval"`
	Timeout     time.Duration `yaml:"timeout"`
	TopicPrefix string        `yaml:"topic_prefix"`
	QoSLevels   []int         `yaml:"qos_levels"`
}

// AlertsConfig holds Teams webhook and alert rule definitions.
type AlertsConfig struct {
	TeamsWebhookURL string             `yaml:"teams_webhook_url"`
	HourlyReport    HourlyReportConfig `yaml:"hourly_report"`
	Rules           []AlertRuleConfig  `yaml:"rules"`
}

// HourlyReportConfig controls the periodic status digest.
type HourlyReportConfig struct {
	Enabled            bool          `yaml:"enabled"`
	Interval           time.Duration `yaml:"interval"`
	IncludeWhenHealthy bool          `yaml:"include_when_healthy"`
}

// AlertRuleConfig defines a single threshold-based alert rule.
type AlertRuleConfig struct {
	Name      string        `yaml:"name"`
	Condition string        `yaml:"condition"`
	Threshold float64       `yaml:"threshold"`
	Operator  string        `yaml:"operator"`
	Severity  string        `yaml:"severity"`
	Window    time.Duration `yaml:"window"`
}

// StorageConfig holds durable storage settings.
type StorageConfig struct {
	AzureBlob AzureBlobConfig `yaml:"azure_blob"`
}

// AzureBlobConfig holds Azure Blob Storage connection details.
type AzureBlobConfig struct {
	Enabled       bool   `yaml:"enabled"`
	Container     string `yaml:"container"`
	AccountName   string `yaml:"account_name"`
	AccountKey    string `yaml:"account_key"`
	RetentionDays int    `yaml:"retention_days"`
}

// ServerConfig holds the embedded web server settings.
type ServerConfig struct {
	Port int `yaml:"port"`
}

// Load reads a YAML config file and expands ${ENV_VAR} references.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config file: %w", err)
	}

	expanded := os.Expand(string(data), func(key string) string {
		if val, ok := os.LookupEnv(key); ok {
			return val
		}
		return "${" + key + "}"
	})

	cfg := &Config{
		Alerts: AlertsConfig{
			HourlyReport: HourlyReportConfig{
				Enabled:            true,
				Interval:           1 * time.Hour,
				IncludeWhenHealthy: true,
			},
		},
	}
	if err := yaml.Unmarshal([]byte(expanded), cfg); err != nil {
		return nil, fmt.Errorf("parsing config: %w", err)
	}

	setDefaults(cfg)

	if err := validate(cfg); err != nil {
		return nil, err
	}

	return cfg, nil
}

func setDefaults(cfg *Config) {
	if cfg.Broker.Port == 0 {
		cfg.Broker.Port = 8883
	}
	if cfg.Broker.ClientID == "" {
		hostname, _ := os.Hostname()
		cfg.Broker.ClientID = "hivemq-canary-" + hostname
	}
	if cfg.Canary.Interval == 0 {
		cfg.Canary.Interval = 30 * time.Second
	}
	if cfg.Canary.Timeout == 0 {
		cfg.Canary.Timeout = 10 * time.Second
	}
	if cfg.Canary.TopicPrefix == "" {
		cfg.Canary.TopicPrefix = "monitor/canary"
	}
	if len(cfg.Canary.QoSLevels) == 0 {
		cfg.Canary.QoSLevels = []int{0, 1}
	}
	if cfg.Server.Port == 0 {
		cfg.Server.Port = 8080
	}
	if cfg.Storage.AzureBlob.RetentionDays == 0 {
		cfg.Storage.AzureBlob.RetentionDays = 90
	}

	for i := range cfg.Alerts.Rules {
		if cfg.Alerts.Rules[i].Operator == "" {
			cfg.Alerts.Rules[i].Operator = ">"
		}
		if cfg.Alerts.Rules[i].Severity == "" {
			cfg.Alerts.Rules[i].Severity = "warning"
		}
	}
}

func validate(cfg *Config) error {
	var errs []string

	if cfg.Broker.Host == "" {
		errs = append(errs, "broker.host is required")
	}
	if cfg.Broker.Username == "" {
		errs = append(errs, "broker.username is required")
	}
	if cfg.Broker.Password == "" {
		errs = append(errs, "broker.password is required")
	}
	if cfg.Alerts.TeamsWebhookURL == "" {
		errs = append(errs, "alerts.teams_webhook_url is required")
	}

	for i, rule := range cfg.Alerts.Rules {
		if rule.Name == "" {
			errs = append(errs, fmt.Sprintf("alerts.rules[%d].name is required", i))
		}
		if rule.Condition == "" {
			errs = append(errs, fmt.Sprintf("alerts.rules[%d].condition is required", i))
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("config validation errors:\n  %s", strings.Join(errs, "\n  "))
	}
	return nil
}
