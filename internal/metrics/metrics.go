package metrics

import (
	"math"
	"sort"
	"sync"
	"time"
)

// Sample represents a single canary check result.
type Sample struct {
	Timestamp          time.Time `json:"timestamp"`
	Connected          bool      `json:"connected"`
	ConnectDurationMs  float64   `json:"connect_duration_ms"`
	RoundTripMs        float64   `json:"round_trip_ms"`
	QoS                byte      `json:"qos"`
	MessageSent        bool      `json:"message_sent"`
	MessageReceived    bool      `json:"message_received"`
	Error              string    `json:"error,omitempty"`
	ReconnectOccurred  bool      `json:"reconnect_occurred"`
}

// Snapshot is a point-in-time summary of canary health, used for alerting and reporting.
type Snapshot struct {
	Timestamp            time.Time `json:"timestamp"`
	Connected            bool      `json:"connected"`
	ConsecutiveFailures  int       `json:"consecutive_failures"`
	RoundTripAvgMs       float64   `json:"round_trip_avg_ms"`
	RoundTripP95Ms       float64   `json:"round_trip_p95_ms"`
	RoundTripMaxMs       float64   `json:"round_trip_max_ms"`
	DeliverySuccessRate  float64   `json:"delivery_success_rate"`
	MessagesSent         int       `json:"messages_sent"`
	MessagesReceived     int       `json:"messages_received"`
	ReconnectCount       int       `json:"reconnect_count"`
	UptimePercent        float64   `json:"uptime_percent"`
	LastError            string    `json:"last_error,omitempty"`
	WindowDuration       string    `json:"window_duration"`
}

// MetricsWriter is the interface for adding samples to a buffer.
type MetricsWriter interface {
	Add(Sample)
}

// MetricsReader is the interface for reading samples and computing snapshots.
type MetricsReader interface {
	All() []Sample
	Since(time.Time) []Sample
	Latest() *Sample
	Count() int
	SnapshotSince(time.Time) Snapshot
}

// RingBuffer stores samples in a fixed-size circular buffer.
// It is safe for concurrent use.
type RingBuffer struct {
	mu      sync.RWMutex
	samples []Sample
	size    int
	head    int
	count   int
}

// NewRingBuffer creates a ring buffer that holds the given number of samples.
// With a 30s interval, 2880 samples = 24 hours of data.
func NewRingBuffer(size int) *RingBuffer {
	return &RingBuffer{
		samples: make([]Sample, size),
		size:    size,
	}
}

// Add inserts a new sample into the ring buffer.
func (rb *RingBuffer) Add(s Sample) {
	rb.mu.Lock()
	defer rb.mu.Unlock()

	rb.samples[rb.head] = s
	rb.head = (rb.head + 1) % rb.size
	if rb.count < rb.size {
		rb.count++
	}
}

// All returns all samples in chronological order.
func (rb *RingBuffer) All() []Sample {
	rb.mu.RLock()
	defer rb.mu.RUnlock()

	result := make([]Sample, 0, rb.count)
	start := (rb.head - rb.count + rb.size) % rb.size
	for i := 0; i < rb.count; i++ {
		idx := (start + i) % rb.size
		result = append(result, rb.samples[idx])
	}
	return result
}

// Since returns samples from the given time onward.
func (rb *RingBuffer) Since(t time.Time) []Sample {
	all := rb.All()
	result := make([]Sample, 0)
	for _, s := range all {
		if !s.Timestamp.Before(t) {
			result = append(result, s)
		}
	}
	return result
}

// Latest returns the most recent sample, or nil if empty.
func (rb *RingBuffer) Latest() *Sample {
	rb.mu.RLock()
	defer rb.mu.RUnlock()

	if rb.count == 0 {
		return nil
	}
	idx := (rb.head - 1 + rb.size) % rb.size
	s := rb.samples[idx]
	return &s
}

// Count returns the number of samples currently stored.
func (rb *RingBuffer) Count() int {
	rb.mu.RLock()
	defer rb.mu.RUnlock()
	return rb.count
}

// SnapshotSince computes an aggregate snapshot from samples since the given time.
func (rb *RingBuffer) SnapshotSince(since time.Time) Snapshot {
	samples := rb.Since(since)
	now := time.Now().UTC()
	window := now.Sub(since)

	snap := Snapshot{
		Timestamp:      now,
		WindowDuration: window.String(),
	}

	if len(samples) == 0 {
		return snap
	}

	var (
		roundTrips     []float64
		sent, received int
		connected      int
		reconnects     int
		lastError      string
	)

	// Count consecutive failures from the tail
	consecutiveFailures := 0
	for i := len(samples) - 1; i >= 0; i-- {
		if !samples[i].Connected || !samples[i].MessageReceived {
			consecutiveFailures++
		} else {
			break
		}
	}

	for _, s := range samples {
		if s.Connected {
			connected++
		}
		if s.RoundTripMs > 0 {
			roundTrips = append(roundTrips, s.RoundTripMs)
		}
		if s.MessageSent {
			sent++
		}
		if s.MessageReceived {
			received++
		}
		if s.ReconnectOccurred {
			reconnects++
		}
		if s.Error != "" {
			lastError = s.Error
		}
	}

	snap.Connected = len(samples) > 0 && samples[len(samples)-1].Connected
	snap.ConsecutiveFailures = consecutiveFailures
	snap.MessagesSent = sent
	snap.MessagesReceived = received
	snap.ReconnectCount = reconnects
	snap.LastError = lastError

	if sent > 0 {
		snap.DeliverySuccessRate = float64(received) / float64(sent)
	}

	if len(samples) > 0 {
		snap.UptimePercent = float64(connected) / float64(len(samples)) * 100.0
	}

	if len(roundTrips) > 0 {
		snap.RoundTripAvgMs = avg(roundTrips)
		snap.RoundTripP95Ms = percentile(roundTrips, 95)
		snap.RoundTripMaxMs = maxVal(roundTrips)
	}

	return snap
}

func avg(vals []float64) float64 {
	if len(vals) == 0 {
		return 0
	}
	sum := 0.0
	for _, v := range vals {
		sum += v
	}
	return math.Round(sum/float64(len(vals))*100) / 100
}

func percentile(vals []float64, p float64) float64 {
	if len(vals) == 0 {
		return 0
	}
	sorted := make([]float64, len(vals))
	copy(sorted, vals)
	sort.Float64s(sorted)

	rank := p / 100.0 * float64(len(sorted)-1)
	lower := int(math.Floor(rank))
	upper := int(math.Ceil(rank))
	if lower == upper {
		return math.Round(sorted[lower]*100) / 100
	}
	weight := rank - float64(lower)
	result := sorted[lower]*(1-weight) + sorted[upper]*weight
	return math.Round(result*100) / 100
}

func maxVal(vals []float64) float64 {
	if len(vals) == 0 {
		return 0
	}
	m := vals[0]
	for _, v := range vals[1:] {
		if v > m {
			m = v
		}
	}
	return math.Round(m*100) / 100
}
