package metrics

import (
	"sync"
	"testing"
	"time"
)

// baseTime is a fixed reference point used across tests to keep timestamps
// deterministic and unaffected by wall-clock drift.
var baseTime = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// makeSample constructs a Sample at baseTime offset by the given duration,
// with sensible defaults for a healthy check.
func makeSample(offset time.Duration, opts ...func(*Sample)) Sample {
	s := Sample{
		Timestamp:       baseTime.Add(offset),
		Connected:       true,
		MessageSent:     true,
		MessageReceived: true,
		RoundTripMs:     10.0,
	}
	for _, o := range opts {
		o(&s)
	}
	return s
}

// ---- RingBuffer.Add / All -----------------------------------------------

func TestRingBuffer_Add_And_All(t *testing.T) {
	const capacity = 5
	rb := NewRingBuffer(capacity)

	for i := 0; i < capacity; i++ {
		rb.Add(makeSample(time.Duration(i) * time.Second))
	}

	all := rb.All()

	if len(all) != capacity {
		t.Fatalf("All() returned %d samples, want %d", len(all), capacity)
	}

	for i, s := range all {
		want := baseTime.Add(time.Duration(i) * time.Second)
		if !s.Timestamp.Equal(want) {
			t.Errorf("sample[%d].Timestamp = %v, want %v", i, s.Timestamp, want)
		}
	}
}

// ---- RingBuffer wraparound -----------------------------------------------

func TestRingBuffer_Wraparound(t *testing.T) {
	const capacity = 3
	rb := NewRingBuffer(capacity)

	// Add capacity+2 samples: the first two should be evicted.
	total := capacity + 2
	for i := 0; i < total; i++ {
		rb.Add(makeSample(time.Duration(i) * time.Second))
	}

	all := rb.All()

	if len(all) != capacity {
		t.Fatalf("All() returned %d samples after wraparound, want %d", len(all), capacity)
	}

	// Oldest surviving sample should be the one at index 2 (0-based).
	wantStart := baseTime.Add(2 * time.Second)
	if !all[0].Timestamp.Equal(wantStart) {
		t.Errorf("oldest sample timestamp = %v, want %v", all[0].Timestamp, wantStart)
	}

	// Samples must still be in chronological order.
	for i := 1; i < len(all); i++ {
		if !all[i].Timestamp.After(all[i-1].Timestamp) {
			t.Errorf("sample[%d] (%v) is not after sample[%d] (%v)",
				i, all[i].Timestamp, i-1, all[i-1].Timestamp)
		}
	}
}

// ---- RingBuffer.Latest ---------------------------------------------------

func TestRingBuffer_Latest(t *testing.T) {
	t.Run("empty buffer returns nil", func(t *testing.T) {
		rb := NewRingBuffer(5)
		if rb.Latest() != nil {
			t.Error("Latest() on empty buffer should return nil")
		}
	})

	t.Run("returns most recently added sample", func(t *testing.T) {
		rb := NewRingBuffer(5)

		for i := 0; i < 3; i++ {
			rb.Add(makeSample(time.Duration(i) * time.Second))
		}

		latest := rb.Latest()
		if latest == nil {
			t.Fatal("Latest() returned nil, expected a sample")
		}

		wantTS := baseTime.Add(2 * time.Second)
		if !latest.Timestamp.Equal(wantTS) {
			t.Errorf("Latest().Timestamp = %v, want %v", latest.Timestamp, wantTS)
		}
	})

	t.Run("returns most recent after wraparound", func(t *testing.T) {
		const capacity = 3
		rb := NewRingBuffer(capacity)

		total := capacity + 2
		for i := 0; i < total; i++ {
			rb.Add(makeSample(time.Duration(i) * time.Second))
		}

		latest := rb.Latest()
		if latest == nil {
			t.Fatal("Latest() returned nil after wraparound")
		}

		wantTS := baseTime.Add(time.Duration(total-1) * time.Second)
		if !latest.Timestamp.Equal(wantTS) {
			t.Errorf("Latest().Timestamp = %v, want %v", latest.Timestamp, wantTS)
		}
	})
}

// ---- RingBuffer.Since ----------------------------------------------------

func TestRingBuffer_Since(t *testing.T) {
	rb := NewRingBuffer(10)

	// Add samples at t+0s, t+10s, t+20s, t+30s, t+40s.
	offsets := []time.Duration{0, 10, 20, 30, 40}
	for _, off := range offsets {
		rb.Add(makeSample(off * time.Second))
	}

	t.Run("returns all samples at or after cutoff", func(t *testing.T) {
		cutoff := baseTime.Add(20 * time.Second) // inclusive
		got := rb.Since(cutoff)

		if len(got) != 3 {
			t.Fatalf("Since(t+20s) returned %d samples, want 3", len(got))
		}
		if !got[0].Timestamp.Equal(baseTime.Add(20*time.Second)) {
			t.Errorf("first sample = %v, want t+20s", got[0].Timestamp)
		}
	})

	t.Run("returns empty slice when cutoff is in the future", func(t *testing.T) {
		cutoff := baseTime.Add(100 * time.Second)
		got := rb.Since(cutoff)
		if len(got) != 0 {
			t.Errorf("Since(future) returned %d samples, want 0", len(got))
		}
	})

	t.Run("returns all samples when cutoff is before oldest", func(t *testing.T) {
		cutoff := baseTime.Add(-1 * time.Second)
		got := rb.Since(cutoff)
		if len(got) != len(offsets) {
			t.Errorf("Since(before oldest) returned %d samples, want %d", len(got), len(offsets))
		}
	})
}

// ---- RingBuffer.Count ---------------------------------------------------

func TestRingBuffer_Count(t *testing.T) {
	t.Run("zero on empty buffer", func(t *testing.T) {
		rb := NewRingBuffer(5)
		if rb.Count() != 0 {
			t.Errorf("Count() = %d on empty buffer, want 0", rb.Count())
		}
	})

	t.Run("increments up to capacity", func(t *testing.T) {
		const capacity = 4
		rb := NewRingBuffer(capacity)

		for i := 0; i < capacity; i++ {
			rb.Add(makeSample(time.Duration(i) * time.Second))
			if rb.Count() != i+1 {
				t.Errorf("after %d adds: Count() = %d, want %d", i+1, rb.Count(), i+1)
			}
		}
	})

	t.Run("stays at capacity after wraparound", func(t *testing.T) {
		const capacity = 3
		rb := NewRingBuffer(capacity)

		for i := 0; i < capacity*3; i++ {
			rb.Add(makeSample(time.Duration(i) * time.Second))
		}

		if rb.Count() != capacity {
			t.Errorf("Count() = %d after wraparound, want %d", rb.Count(), capacity)
		}
	})
}

// ---- SnapshotSince: empty buffer ----------------------------------------

func TestSnapshotSince_Empty(t *testing.T) {
	rb := NewRingBuffer(10)
	snap := rb.SnapshotSince(baseTime)

	if snap.Connected {
		t.Error("Connected should be false for empty snapshot")
	}
	if snap.ConsecutiveFailures != 0 {
		t.Errorf("ConsecutiveFailures = %d, want 0", snap.ConsecutiveFailures)
	}
	if snap.RoundTripAvgMs != 0 {
		t.Errorf("RoundTripAvgMs = %f, want 0", snap.RoundTripAvgMs)
	}
	if snap.RoundTripP95Ms != 0 {
		t.Errorf("RoundTripP95Ms = %f, want 0", snap.RoundTripP95Ms)
	}
	if snap.RoundTripMaxMs != 0 {
		t.Errorf("RoundTripMaxMs = %f, want 0", snap.RoundTripMaxMs)
	}
	if snap.DeliverySuccessRate != 0 {
		t.Errorf("DeliverySuccessRate = %f, want 0", snap.DeliverySuccessRate)
	}
	if snap.MessagesSent != 0 {
		t.Errorf("MessagesSent = %d, want 0", snap.MessagesSent)
	}
	if snap.MessagesReceived != 0 {
		t.Errorf("MessagesReceived = %d, want 0", snap.MessagesReceived)
	}
	if snap.UptimePercent != 0 {
		t.Errorf("UptimePercent = %f, want 0", snap.UptimePercent)
	}
	if snap.WindowDuration == "" {
		t.Error("WindowDuration should be non-empty even for an empty snapshot")
	}
}

// ---- SnapshotSince: single sample ---------------------------------------

func TestSnapshotSince_SingleSample(t *testing.T) {
	rb := NewRingBuffer(10)

	sample := makeSample(time.Second, func(s *Sample) {
		s.RoundTripMs = 42.0
		s.ReconnectOccurred = true
		s.Error = "transient error"
	})
	rb.Add(sample)

	// Use a cutoff before the sample so it is included.
	snap := rb.SnapshotSince(baseTime)

	if !snap.Connected {
		t.Error("Connected should be true")
	}
	if snap.ConsecutiveFailures != 0 {
		t.Errorf("ConsecutiveFailures = %d, want 0 (sample is healthy)", snap.ConsecutiveFailures)
	}
	if snap.MessagesSent != 1 {
		t.Errorf("MessagesSent = %d, want 1", snap.MessagesSent)
	}
	if snap.MessagesReceived != 1 {
		t.Errorf("MessagesReceived = %d, want 1", snap.MessagesReceived)
	}
	if snap.DeliverySuccessRate != 1.0 {
		t.Errorf("DeliverySuccessRate = %f, want 1.0", snap.DeliverySuccessRate)
	}
	if snap.RoundTripAvgMs != 42.0 {
		t.Errorf("RoundTripAvgMs = %f, want 42.0", snap.RoundTripAvgMs)
	}
	if snap.RoundTripP95Ms != 42.0 {
		t.Errorf("RoundTripP95Ms = %f, want 42.0", snap.RoundTripP95Ms)
	}
	if snap.RoundTripMaxMs != 42.0 {
		t.Errorf("RoundTripMaxMs = %f, want 42.0", snap.RoundTripMaxMs)
	}
	if snap.UptimePercent != 100.0 {
		t.Errorf("UptimePercent = %f, want 100.0", snap.UptimePercent)
	}
	if snap.ReconnectCount != 1 {
		t.Errorf("ReconnectCount = %d, want 1", snap.ReconnectCount)
	}
	if snap.LastError != "transient error" {
		t.Errorf("LastError = %q, want %q", snap.LastError, "transient error")
	}
}

// ---- SnapshotSince: consecutive failures ---------------------------------

func TestSnapshotSince_ConsecutiveFailures(t *testing.T) {
	tests := []struct {
		name    string
		samples []Sample
		want    int
	}{
		{
			name: "no failures",
			samples: []Sample{
				makeSample(1*time.Second),
				makeSample(2*time.Second),
				makeSample(3*time.Second),
			},
			want: 0,
		},
		{
			name: "all failures",
			samples: []Sample{
				makeSample(1*time.Second, func(s *Sample) { s.Connected = false }),
				makeSample(2*time.Second, func(s *Sample) { s.Connected = false }),
			},
			want: 2,
		},
		{
			name: "failures only at tail — stops at first success",
			samples: []Sample{
				makeSample(1*time.Second), // success
				makeSample(2*time.Second, func(s *Sample) { s.MessageReceived = false }), // fail
				makeSample(3*time.Second, func(s *Sample) { s.Connected = false }),        // fail
			},
			want: 2,
		},
		{
			name: "success breaks the streak",
			samples: []Sample{
				makeSample(1*time.Second, func(s *Sample) { s.Connected = false }), // fail
				makeSample(2*time.Second),                                           // success — breaks streak
				makeSample(3*time.Second, func(s *Sample) { s.Connected = false }), // fail
			},
			want: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rb := NewRingBuffer(10)
			for _, s := range tt.samples {
				rb.Add(s)
			}
			snap := rb.SnapshotSince(baseTime)
			if snap.ConsecutiveFailures != tt.want {
				t.Errorf("ConsecutiveFailures = %d, want %d", snap.ConsecutiveFailures, tt.want)
			}
		})
	}
}

// ---- SnapshotSince: P95 percentile --------------------------------------

func TestSnapshotSince_P95(t *testing.T) {
	// Add 100 samples with RoundTripMs values 1..100.
	// P95 via linear interpolation on sorted slice [1..100]:
	//   rank = 0.95 * 99 = 94.05
	//   lower=94 -> value 95, upper=95 -> value 96
	//   result = 95*(1-0.05) + 96*0.05 = 90.25 + 4.8 = 95.05
	const wantP95 = 95.05

	rb := NewRingBuffer(200)
	for i := 1; i <= 100; i++ {
		s := makeSample(time.Duration(i)*time.Second, func(s *Sample) {
			s.RoundTripMs = float64(i)
		})
		// Capture loop variable before the closure executes.
		s.RoundTripMs = float64(i)
		rb.Add(s)
	}

	snap := rb.SnapshotSince(baseTime)

	if snap.RoundTripP95Ms != wantP95 {
		t.Errorf("RoundTripP95Ms = %f, want %f", snap.RoundTripP95Ms, wantP95)
	}

	// Avg of 1..100 = 50.5
	const wantAvg = 50.5
	if snap.RoundTripAvgMs != wantAvg {
		t.Errorf("RoundTripAvgMs = %f, want %f", snap.RoundTripAvgMs, wantAvg)
	}

	// Max should be 100.
	if snap.RoundTripMaxMs != 100.0 {
		t.Errorf("RoundTripMaxMs = %f, want 100.0", snap.RoundTripMaxMs)
	}
}

// ---- SnapshotSince: delivery rate ---------------------------------------

func TestSnapshotSince_DeliveryRate(t *testing.T) {
	tests := []struct {
		name     string
		sent     int
		received int
		wantRate float64
	}{
		{"all delivered", 4, 4, 1.0},
		{"half delivered", 4, 2, 0.5},
		{"none sent", 0, 0, 0.0},
		{"none received", 3, 0, 0.0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rb := NewRingBuffer(20)

			for i := 0; i < tt.sent; i++ {
				delivered := i < tt.received
				s := makeSample(time.Duration(i)*time.Second, func(s *Sample) {
					s.MessageSent = true
					s.MessageReceived = delivered
				})
				rb.Add(s)
			}
			// When nothing was sent, add a neutral sample so the window is non-empty.
			if tt.sent == 0 {
				rb.Add(makeSample(0, func(s *Sample) {
					s.MessageSent = false
					s.MessageReceived = false
				}))
			}

			snap := rb.SnapshotSince(baseTime)

			if snap.DeliverySuccessRate != tt.wantRate {
				t.Errorf("DeliverySuccessRate = %f, want %f", snap.DeliverySuccessRate, tt.wantRate)
			}
		})
	}
}

// ---- RingBuffer: thread safety -------------------------------------------

// TestRingBuffer_ThreadSafety exercises concurrent Add/All/Latest from
// multiple goroutines. Run with -race to detect data races.
func TestRingBuffer_ThreadSafety(t *testing.T) {
	const (
		capacity   = 50
		goroutines = 8
		addsEach   = 200
	)

	rb := NewRingBuffer(capacity)

	var wg sync.WaitGroup

	// Writers.
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for i := 0; i < addsEach; i++ {
				rb.Add(makeSample(time.Duration(id*addsEach+i) * time.Millisecond))
			}
		}(g)
	}

	// Concurrent readers — All.
	for g := 0; g < goroutines/2; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < addsEach; i++ {
				_ = rb.All()
			}
		}()
	}

	// Concurrent readers — Latest.
	for g := 0; g < goroutines/2; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < addsEach; i++ {
				_ = rb.Latest()
			}
		}()
	}

	wg.Wait()

	// After all writes, count must equal capacity (buffer is full).
	if rb.Count() != capacity {
		t.Errorf("Count() = %d after concurrent writes, want %d", rb.Count(), capacity)
	}

	// All() must return exactly capacity samples (no corruption from concurrent access).
	all := rb.All()
	if len(all) != capacity {
		t.Fatalf("All() returned %d samples, want %d", len(all), capacity)
	}

	// Verify no zero-value timestamps (all samples were written by goroutines with real timestamps).
	for i, s := range all {
		if s.Timestamp.IsZero() {
			t.Errorf("sample %d has zero timestamp", i)
		}
	}
}
