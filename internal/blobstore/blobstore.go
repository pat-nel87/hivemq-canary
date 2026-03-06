package blobstore

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"hivemq-canary/internal/config"
	"hivemq-canary/internal/metrics"
)

// BlobWriter abstracts Azure Blob Storage append operations for testability.
type BlobWriter interface {
	CreateIfNotExists(ctx context.Context, blobName string) error
	AppendBlock(ctx context.Context, blobName string, data []byte) error
	DeleteBlob(ctx context.Context, blobName string) error
	ListBlobs(ctx context.Context, prefix string) ([]string, error)
}

// Store writes metric samples to Azure Blob Storage as daily append blobs.
// Each day gets a single JSONL file: metrics/YYYY-MM-DD.jsonl
type Store struct {
	cfg    config.AzureBlobConfig
	logger *slog.Logger
	writer BlobWriter

	mu          sync.Mutex
	writeBuffer [][]byte
	createdBlobs map[string]bool // track daily blob creation
}

// New creates a new blob store. If enabled, initializes the Azure SDK client.
func New(cfg config.AzureBlobConfig, logger *slog.Logger) *Store {
	s := &Store{
		cfg:          cfg,
		logger:       logger,
		createdBlobs: make(map[string]bool),
	}

	if cfg.Enabled {
		writer, err := NewAzureBlobWriter(cfg)
		if err != nil {
			logger.Error("failed to initialize Azure Blob writer, storage will be disabled", "error", err)
			return s
		}
		s.writer = writer
	}

	return s
}

// NewWithWriter creates a blob store with a custom BlobWriter (for testing).
func NewWithWriter(cfg config.AzureBlobConfig, logger *slog.Logger, writer BlobWriter) *Store {
	s := New(cfg, logger)
	s.writer = writer
	return s
}

// blobName returns the blob path for the given timestamp.
func blobName(t time.Time) string {
	return fmt.Sprintf("metrics/%s.jsonl", t.UTC().Format("2006-01-02"))
}

// WriteSample serializes a sample to JSON and appends it to today's blob.
func (s *Store) WriteSample(ctx context.Context, sample metrics.Sample) error {
	if !s.cfg.Enabled {
		return nil
	}

	line, err := json.Marshal(sample)
	if err != nil {
		return fmt.Errorf("marshaling sample: %w", err)
	}
	line = append(line, '\n')

	s.mu.Lock()
	s.writeBuffer = append(s.writeBuffer, line)
	s.mu.Unlock()

	return nil
}

// WriteSnapshot serializes a snapshot to JSON and appends it to today's blob.
func (s *Store) WriteSnapshot(ctx context.Context, snap metrics.Snapshot) error {
	if !s.cfg.Enabled {
		return nil
	}

	line, err := json.Marshal(snap)
	if err != nil {
		return fmt.Errorf("marshaling snapshot: %w", err)
	}
	line = append(line, '\n')

	s.mu.Lock()
	s.writeBuffer = append(s.writeBuffer, line)
	s.mu.Unlock()

	return nil
}

// Flush writes buffered data to Azure Blob Storage.
// This should be called periodically (e.g. every minute) to batch writes.
func (s *Store) Flush(ctx context.Context) error {
	if !s.cfg.Enabled {
		return nil
	}

	s.mu.Lock()
	if len(s.writeBuffer) == 0 {
		s.mu.Unlock()
		return nil
	}
	buf := s.writeBuffer
	s.writeBuffer = nil
	s.mu.Unlock()

	// Combine all buffered lines
	combined := bytes.Join(buf, nil)

	blob := blobName(time.Now())

	if s.writer == nil {
		s.logger.Debug("blob writer not configured, discarding flush",
			"blob", blob,
			"lines", len(buf),
			"bytes", len(combined),
		)
		return nil
	}

	// Create the blob if it doesn't exist (once per day)
	if !s.createdBlobs[blob] {
		if err := s.writer.CreateIfNotExists(ctx, blob); err != nil {
			return fmt.Errorf("creating blob %s: %w", blob, err)
		}
		s.createdBlobs[blob] = true
	}

	if err := s.writer.AppendBlock(ctx, blob, combined); err != nil {
		return fmt.Errorf("appending to blob %s: %w", blob, err)
	}

	s.logger.Debug("flushed metrics to blob",
		"blob", blob,
		"lines", len(buf),
		"bytes", len(combined),
	)

	return nil
}

// CleanupOldBlobs deletes blobs older than the configured retention period.
func (s *Store) CleanupOldBlobs(ctx context.Context) error {
	if !s.cfg.Enabled || s.writer == nil {
		return nil
	}

	cutoff := time.Now().UTC().AddDate(0, 0, -s.cfg.RetentionDays)
	blobs, err := s.writer.ListBlobs(ctx, "metrics/")
	if err != nil {
		return fmt.Errorf("listing blobs for cleanup: %w", err)
	}

	for _, name := range blobs {
		dateStr := blobDateStr(name)
		blobDate, err := time.Parse("2006-01-02", dateStr)
		if err != nil {
			continue // skip blobs with unexpected names
		}
		if blobDate.Before(cutoff) {
			if err := s.writer.DeleteBlob(ctx, name); err != nil {
				s.logger.Error("failed to delete old blob", "blob", name, "error", err)
				continue
			}
			s.logger.Info("deleted old blob", "blob", name)
		}
	}

	return nil
}

// StartFlusher runs a periodic flush loop and daily retention cleanup.
func (s *Store) StartFlusher(ctx context.Context, interval time.Duration) {
	if !s.cfg.Enabled {
		s.logger.Info("blob storage disabled, skipping flusher")
		return
	}

	// Run retention cleanup on startup
	if err := s.CleanupOldBlobs(ctx); err != nil {
		s.logger.Error("startup retention cleanup failed", "error", err)
	}

	flushTicker := time.NewTicker(interval)
	defer flushTicker.Stop()

	cleanupTicker := time.NewTicker(24 * time.Hour)
	defer cleanupTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			// Final flush on shutdown
			if err := s.Flush(context.Background()); err != nil {
				s.logger.Error("final flush failed", "error", err)
			}
			return
		case <-flushTicker.C:
			if err := s.Flush(ctx); err != nil {
				s.logger.Error("periodic flush failed", "error", err)
			}
		case <-cleanupTicker.C:
			if err := s.CleanupOldBlobs(ctx); err != nil {
				s.logger.Error("retention cleanup failed", "error", err)
			}
		}
	}
}
