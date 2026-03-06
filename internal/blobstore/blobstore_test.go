package blobstore

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"hivemq-canary/internal/config"
	"hivemq-canary/internal/metrics"
)

type mockBlobWriter struct {
	mu       sync.Mutex
	created  []string
	appended map[string][]byte
	deleted  []string
	blobs    []string // returned by ListBlobs

	createErr error
	appendErr error
}

func newMockBlobWriter() *mockBlobWriter {
	return &mockBlobWriter{
		appended: make(map[string][]byte),
	}
}

func (m *mockBlobWriter) CreateIfNotExists(_ context.Context, blobName string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.createErr != nil {
		return m.createErr
	}
	m.created = append(m.created, blobName)
	return nil
}

func (m *mockBlobWriter) AppendBlock(_ context.Context, blobName string, data []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.appendErr != nil {
		return m.appendErr
	}
	m.appended[blobName] = append(m.appended[blobName], data...)
	return nil
}

func (m *mockBlobWriter) DeleteBlob(_ context.Context, blobName string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.deleted = append(m.deleted, blobName)
	return nil
}

func (m *mockBlobWriter) ListBlobs(_ context.Context, _ string) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.blobs, nil
}

func enabledCfg() config.AzureBlobConfig {
	return config.AzureBlobConfig{
		Enabled:       true,
		Container:     "test",
		AccountName:   "test",
		RetentionDays: 90,
	}
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

func TestWriteSample_Disabled(t *testing.T) {
	s := New(config.AzureBlobConfig{Enabled: false}, testLogger())
	err := s.WriteSample(context.Background(), metrics.Sample{Timestamp: time.Now()})
	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
}

func TestWriteSample_Buffers(t *testing.T) {
	mock := newMockBlobWriter()
	s := NewWithWriter(enabledCfg(), testLogger(), mock)

	sample := metrics.Sample{Timestamp: time.Now(), Connected: true, RoundTripMs: 42.5}
	if err := s.WriteSample(context.Background(), sample); err != nil {
		t.Fatalf("WriteSample failed: %v", err)
	}

	s.mu.Lock()
	bufLen := len(s.writeBuffer)
	s.mu.Unlock()

	if bufLen != 1 {
		t.Fatalf("expected 1 buffered line, got %d", bufLen)
	}
}

func TestFlush_Disabled(t *testing.T) {
	s := New(config.AzureBlobConfig{Enabled: false}, testLogger())
	if err := s.Flush(context.Background()); err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
}

func TestFlush_EmptyBuffer(t *testing.T) {
	mock := newMockBlobWriter()
	s := NewWithWriter(enabledCfg(), testLogger(), mock)
	if err := s.Flush(context.Background()); err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if len(mock.created) != 0 {
		t.Fatal("expected no blob creation on empty buffer")
	}
}

func TestFlush_WritesToBlob(t *testing.T) {
	mock := newMockBlobWriter()
	s := NewWithWriter(enabledCfg(), testLogger(), mock)

	sample := metrics.Sample{Timestamp: time.Now().UTC(), Connected: true, RoundTripMs: 10}
	s.WriteSample(context.Background(), sample)
	s.WriteSample(context.Background(), sample)

	if err := s.Flush(context.Background()); err != nil {
		t.Fatalf("Flush failed: %v", err)
	}

	expectedBlob := blobName(time.Now())
	if len(mock.created) != 1 || mock.created[0] != expectedBlob {
		t.Fatalf("expected blob %s created, got %v", expectedBlob, mock.created)
	}

	data := mock.appended[expectedBlob]
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 lines, got %d", len(lines))
	}

	var decoded metrics.Sample
	if err := json.Unmarshal([]byte(lines[0]), &decoded); err != nil {
		t.Fatalf("failed to unmarshal line: %v", err)
	}
	if decoded.RoundTripMs != 10 {
		t.Fatalf("expected RoundTripMs=10, got %f", decoded.RoundTripMs)
	}
}

func TestFlush_CreateOncePerDay(t *testing.T) {
	mock := newMockBlobWriter()
	s := NewWithWriter(enabledCfg(), testLogger(), mock)

	sample := metrics.Sample{Timestamp: time.Now().UTC()}
	s.WriteSample(context.Background(), sample)
	s.Flush(context.Background())
	s.WriteSample(context.Background(), sample)
	s.Flush(context.Background())

	if len(mock.created) != 1 {
		t.Fatalf("expected 1 create call (cached), got %d", len(mock.created))
	}
}

func TestFlush_NilWriter(t *testing.T) {
	// Construct directly with nil writer (simulates Azure init failure)
	s := &Store{
		cfg:          enabledCfg(),
		logger:       testLogger(),
		createdBlobs: make(map[string]bool),
	}

	sample := metrics.Sample{Timestamp: time.Now().UTC()}
	s.WriteSample(context.Background(), sample)

	// Should not error, just discard
	if err := s.Flush(context.Background()); err != nil {
		t.Fatalf("expected nil error for nil writer, got %v", err)
	}
}

func TestFlush_CreateError(t *testing.T) {
	mock := newMockBlobWriter()
	mock.createErr = fmt.Errorf("create failed")
	s := NewWithWriter(enabledCfg(), testLogger(), mock)

	s.WriteSample(context.Background(), metrics.Sample{Timestamp: time.Now().UTC()})
	err := s.Flush(context.Background())
	if err == nil {
		t.Fatal("expected error from Flush")
	}
	if !strings.Contains(err.Error(), "creating blob") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestFlush_AppendError(t *testing.T) {
	mock := newMockBlobWriter()
	mock.appendErr = fmt.Errorf("append failed")
	s := NewWithWriter(enabledCfg(), testLogger(), mock)

	s.WriteSample(context.Background(), metrics.Sample{Timestamp: time.Now().UTC()})
	err := s.Flush(context.Background())
	if err == nil {
		t.Fatal("expected error from Flush")
	}
	if !strings.Contains(err.Error(), "appending to blob") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestCleanupOldBlobs(t *testing.T) {
	mock := newMockBlobWriter()
	today := time.Now().UTC()
	oldDate := today.AddDate(0, 0, -100) // 100 days ago
	recentDate := today.AddDate(0, 0, -10) // 10 days ago

	mock.blobs = []string{
		fmt.Sprintf("metrics/%s.jsonl", oldDate.Format("2006-01-02")),
		fmt.Sprintf("metrics/%s.jsonl", recentDate.Format("2006-01-02")),
		fmt.Sprintf("metrics/%s.jsonl", today.Format("2006-01-02")),
	}

	s := NewWithWriter(enabledCfg(), testLogger(), mock)
	if err := s.CleanupOldBlobs(context.Background()); err != nil {
		t.Fatalf("CleanupOldBlobs failed: %v", err)
	}

	// Only the 100-day-old blob should be deleted (retention is 90 days)
	if len(mock.deleted) != 1 {
		t.Fatalf("expected 1 deletion, got %d: %v", len(mock.deleted), mock.deleted)
	}
	if !strings.Contains(mock.deleted[0], oldDate.Format("2006-01-02")) {
		t.Fatalf("wrong blob deleted: %s", mock.deleted[0])
	}
}

func TestCleanupOldBlobs_Disabled(t *testing.T) {
	s := New(config.AzureBlobConfig{Enabled: false}, testLogger())
	if err := s.CleanupOldBlobs(context.Background()); err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
}

func TestBlobName(t *testing.T) {
	ts := time.Date(2024, 3, 15, 14, 30, 0, 0, time.UTC)
	got := blobName(ts)
	want := "metrics/2024-03-15.jsonl"
	if got != want {
		t.Fatalf("blobName = %q, want %q", got, want)
	}
}

func TestBlobDateStr(t *testing.T) {
	got := blobDateStr("metrics/2024-03-15.jsonl")
	want := "2024-03-15"
	if got != want {
		t.Fatalf("blobDateStr = %q, want %q", got, want)
	}
}

func TestWriteSnapshot(t *testing.T) {
	mock := newMockBlobWriter()
	s := NewWithWriter(enabledCfg(), testLogger(), mock)

	snap := metrics.Snapshot{
		Timestamp:   time.Now().UTC(),
		Connected:   true,
		UptimePercent: 99.5,
	}
	if err := s.WriteSnapshot(context.Background(), snap); err != nil {
		t.Fatalf("WriteSnapshot failed: %v", err)
	}

	s.mu.Lock()
	bufLen := len(s.writeBuffer)
	s.mu.Unlock()
	if bufLen != 1 {
		t.Fatalf("expected 1 buffered line, got %d", bufLen)
	}
}
