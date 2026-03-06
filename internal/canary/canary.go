package canary

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"sync"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"

	"hivemq-canary/internal/config"
	"hivemq-canary/internal/metrics"
)

// ClientFactory creates an MQTT client from options. Allows injection for testing.
type ClientFactory func(*mqtt.ClientOptions) mqtt.Client

// SampleSink receives samples produced by the canary (e.g. blob storage).
type SampleSink interface {
	WriteSample(ctx context.Context, sample metrics.Sample) error
}

// Canary is the MQTT health check client. It connects to the broker,
// publishes test messages, subscribes for them, and records results.
type Canary struct {
	cfg           config.CanaryConfig
	brokerCfg     config.BrokerConfig
	clientFactory ClientFactory
	client        mqtt.Client
	buffer        metrics.MetricsWriter
	sinks         []SampleSink
	logger        *slog.Logger

	mu              sync.Mutex
	reconnectCount  int
	lastReconnect   time.Time
	pendingMessages map[string]time.Time // correlationID -> sent time
}

// New creates a new Canary. It does not connect until Start is called.
func New(brokerCfg config.BrokerConfig, canaryCfg config.CanaryConfig, buffer metrics.MetricsWriter, logger *slog.Logger) *Canary {
	return &Canary{
		cfg:             canaryCfg,
		brokerCfg:       brokerCfg,
		clientFactory:   mqtt.NewClient,
		buffer:          buffer,
		logger:          logger,
		pendingMessages: make(map[string]time.Time),
	}
}

// NewWithFactory creates a Canary with a custom MQTT client factory (for testing).
func NewWithFactory(brokerCfg config.BrokerConfig, canaryCfg config.CanaryConfig, buffer metrics.MetricsWriter, logger *slog.Logger, factory ClientFactory) *Canary {
	c := New(brokerCfg, canaryCfg, buffer, logger)
	c.clientFactory = factory
	return c
}

// AddSink registers an additional sink to receive samples (e.g. blob storage).
func (c *Canary) AddSink(sink SampleSink) {
	c.sinks = append(c.sinks, sink)
}

// Start connects to the broker and begins canary checks on the configured interval.
// It blocks until the context is cancelled.
func (c *Canary) Start(ctx context.Context) error {
	if err := c.connect(); err != nil {
		// Record the failure but don't exit — we'll retry on the next tick
		c.logger.Error("initial connection failed", "error", err)
		c.buffer.Add(metrics.Sample{
			Timestamp: time.Now().UTC(),
			Error:     fmt.Sprintf("initial connection failed: %v", err),
		})
	}

	ticker := time.NewTicker(c.cfg.Interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			c.logger.Info("canary shutting down")
			c.mu.Lock()
			client := c.client
			c.mu.Unlock()
			if client != nil && client.IsConnected() {
				client.Disconnect(1000)
			}
			return nil
		case <-ticker.C:
			c.runCheck()
		}
	}
}

func (c *Canary) connect() error {
	broker := fmt.Sprintf("tcp://%s:%d", c.brokerCfg.Host, c.brokerCfg.Port)
	if c.brokerCfg.TLS {
		broker = fmt.Sprintf("ssl://%s:%d", c.brokerCfg.Host, c.brokerCfg.Port)
	}

	opts := mqtt.NewClientOptions().
		AddBroker(broker).
		SetClientID(c.brokerCfg.ClientID).
		SetUsername(c.brokerCfg.Username).
		SetPassword(c.brokerCfg.Password).
		SetKeepAlive(30 * time.Second).
		SetAutoReconnect(true).
		SetMaxReconnectInterval(1 * time.Minute).
		SetConnectionLostHandler(c.onConnectionLost).
		SetReconnectingHandler(c.onReconnecting).
		SetOnConnectHandler(c.onConnect).
		SetCleanSession(true).
		SetOrderMatters(false)

	if c.brokerCfg.TLS {
		opts.SetTLSConfig(&tls.Config{
			MinVersion: tls.VersionTLS12,
		})
	}

	client := c.clientFactory(opts)

	c.mu.Lock()
	c.client = client
	c.mu.Unlock()

	connectStart := time.Now()
	token := c.client.Connect()
	if !token.WaitTimeout(c.cfg.Timeout) {
		return fmt.Errorf("connection timeout after %v", c.cfg.Timeout)
	}
	if token.Error() != nil {
		return fmt.Errorf("connection error: %w", token.Error())
	}

	connectDuration := time.Since(connectStart)
	c.logger.Info("connected to broker",
		"host", c.brokerCfg.Host,
		"duration_ms", connectDuration.Milliseconds(),
	)

	return nil
}

func (c *Canary) onConnectionLost(_ mqtt.Client, err error) {
	c.logger.Warn("connection lost", "error", err)
}

func (c *Canary) onReconnecting(_ mqtt.Client, _ *mqtt.ClientOptions) {
	c.mu.Lock()
	c.reconnectCount++
	c.lastReconnect = time.Now()
	c.mu.Unlock()
	c.logger.Info("reconnecting to broker")
}

func (c *Canary) onConnect(client mqtt.Client) {
	// Re-subscribe to canary response topics after reconnect
	topic := fmt.Sprintf("%s/+/response", c.cfg.TopicPrefix)
	token := client.Subscribe(topic, 1, c.onMessage)
	if !token.WaitTimeout(c.cfg.Timeout) {
		c.logger.Error("subscribe timed out after connect", "topic", topic)
	} else if token.Error() != nil {
		c.logger.Error("subscribe failed after connect", "error", token.Error())
	} else {
		c.logger.Info("subscribed to canary response topic", "topic", topic)
	}
}

func (c *Canary) onMessage(_ mqtt.Client, msg mqtt.Message) {
	correlationID := string(msg.Payload())

	c.mu.Lock()
	sentAt, ok := c.pendingMessages[correlationID]
	if ok {
		delete(c.pendingMessages, correlationID)
	}
	c.mu.Unlock()

	if ok {
		roundTrip := time.Since(sentAt)
		c.logger.Debug("canary response received",
			"correlation_id", correlationID,
			"round_trip_ms", roundTrip.Milliseconds(),
		)
	}
}

func (c *Canary) runCheck() {
	ctx := context.Background()
	for _, qos := range c.cfg.QoSLevels {
		sample := c.checkQoS(byte(qos))
		c.buffer.Add(sample)

		for _, sink := range c.sinks {
			if err := sink.WriteSample(ctx, sample); err != nil {
				c.logger.Error("sink write failed", "error", err)
			}
		}

		level := slog.LevelDebug
		if sample.Error != "" {
			level = slog.LevelWarn
		}
		c.logger.Log(ctx, level, "canary check",
			"qos", qos,
			"connected", sample.Connected,
			"round_trip_ms", sample.RoundTripMs,
			"delivered", sample.MessageReceived,
			"error", sample.Error,
		)
	}
}

func (c *Canary) checkQoS(qos byte) metrics.Sample {
	now := time.Now().UTC()
	sample := metrics.Sample{
		Timestamp: now,
		QoS:       qos,
	}

	// Check connection state
	c.mu.Lock()
	client := c.client
	c.mu.Unlock()
	if client == nil || !client.IsConnected() {
		// Try to reconnect
		if err := c.connect(); err != nil {
			sample.Error = fmt.Sprintf("not connected: %v", err)
			return sample
		}
	}

	sample.Connected = true

	// Check if a reconnect happened recently
	c.mu.Lock()
	if !c.lastReconnect.IsZero() && time.Since(c.lastReconnect) < c.cfg.Interval {
		sample.ReconnectOccurred = true
	}
	c.mu.Unlock()

	// Generate correlation ID for round-trip tracking
	correlationID := fmt.Sprintf("%d-%d", now.UnixNano(), qos)

	// Record the pending message
	c.mu.Lock()
	c.pendingMessages[correlationID] = now
	c.mu.Unlock()

	// Subscribe to the specific response topic for this check
	responseTopic := fmt.Sprintf("%s/%s/response", c.cfg.TopicPrefix, correlationID)
	receivedCh := make(chan time.Time, 1)

	c.mu.Lock()
	client = c.client
	c.mu.Unlock()

	subToken := client.Subscribe(responseTopic, qos, func(_ mqtt.Client, msg mqtt.Message) {
		receivedCh <- time.Now()
	})
	if !subToken.WaitTimeout(c.cfg.Timeout) || subToken.Error() != nil {
		sample.Error = "subscribe timeout"
		c.cleanupPending(correlationID)
		return sample
	}
	defer client.Unsubscribe(responseTopic)

	// Publish the canary message
	publishStart := time.Now()
	pubToken := client.Publish(responseTopic, qos, false, []byte(correlationID))
	if !pubToken.WaitTimeout(c.cfg.Timeout) || pubToken.Error() != nil {
		sample.Error = "publish failed"
		c.cleanupPending(correlationID)
		return sample
	}
	sample.MessageSent = true

	// Wait for the message to come back
	select {
	case receivedAt := <-receivedCh:
		sample.MessageReceived = true
		sample.RoundTripMs = float64(receivedAt.Sub(publishStart).Microseconds()) / 1000.0
	case <-time.After(c.cfg.Timeout):
		sample.Error = "round-trip timeout"
	}

	c.cleanupPending(correlationID)
	return sample
}

func (c *Canary) cleanupPending(id string) {
	c.mu.Lock()
	delete(c.pendingMessages, id)
	c.mu.Unlock()
}

// ReconnectCount returns the total number of reconnects since start.
func (c *Canary) ReconnectCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.reconnectCount
}
