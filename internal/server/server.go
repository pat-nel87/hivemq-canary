package server

import (
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"hivemq-canary/internal/alerts"
	"hivemq-canary/internal/config"
	"hivemq-canary/internal/metrics"
	"hivemq-canary/web"
)

// AlertReader provides access to active alert states.
type AlertReader interface {
	ActiveAlerts() map[string]alerts.RuleState
}

// Server is the embedded HTTP server serving the dashboard and JSON API.
type Server struct {
	cfg         config.ServerConfig
	brokerHost  string
	buffer      metrics.MetricsReader
	alertEngine AlertReader
	logger      *slog.Logger
	templates   *template.Template
}

// New creates a new server instance.
func New(
	cfg config.ServerConfig,
	brokerHost string,
	buffer metrics.MetricsReader,
	alertEngine AlertReader,
	logger *slog.Logger,
) *Server {
	return &Server{
		cfg:         cfg,
		brokerHost:  brokerHost,
		buffer:      buffer,
		alertEngine: alertEngine,
		logger:      logger,
	}
}

// Start launches the HTTP server. It blocks until the context is cancelled.
func (s *Server) Start(ctx context.Context) error {
	funcs := template.FuncMap{
		"formatTime": func(t time.Time) string {
			return t.UTC().Format("15:04:05")
		},
		"formatDuration": func(d time.Duration) string {
			return d.Round(time.Second).String()
		},
		"jsonMarshal": func(v any) template.JS {
			b, _ := json.Marshal(v)
			return template.JS(b)
		},
	}

	var err error
	s.templates, err = template.New("").Funcs(funcs).ParseFS(web.TemplateFS, "templates/*.html")
	if err != nil {
		return fmt.Errorf("parsing templates: %w", err)
	}

	r := chi.NewRouter()
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)
	r.Use(middleware.Compress(5))

	// Dashboard
	r.Get("/", s.handleDashboard)

	// API endpoints (for future MCP integration)
	r.Route("/api", func(r chi.Router) {
		r.Get("/health", s.handleHealth)
		r.Get("/metrics/current", s.handleCurrentMetrics)
		r.Get("/metrics/history", s.handleMetricsHistory)
		r.Get("/alerts", s.handleAlerts)
	})

	// Static files (served from embedded FS)
	staticSub, _ := fs.Sub(web.StaticFS, "static")
	r.Handle("/static/*", http.StripPrefix("/static/",
		http.FileServer(http.FS(staticSub)),
	))

	addr := fmt.Sprintf(":%d", s.cfg.Port)
	srv := &http.Server{
		Addr:    addr,
		Handler: r,
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shutdownCtx)
	}()

	s.logger.Info("dashboard server starting", "addr", addr)
	if err := srv.ListenAndServe(); err != http.ErrServerClosed {
		return fmt.Errorf("server error: %w", err)
	}
	return nil
}

// --- Handlers ---

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	snap := s.buffer.SnapshotSince(time.Now().Add(-1 * time.Hour))
	samples := s.buffer.Since(time.Now().Add(-1 * time.Hour))
	activeAlerts := s.alertEngine.ActiveAlerts()

	data := map[string]any{
		"BrokerHost":   s.brokerHost,
		"Snapshot":     snap,
		"Samples":      samples,
		"ActiveAlerts": activeAlerts,
		"UpdatedAt":    time.Now().UTC(),
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.templates.ExecuteTemplate(w, "dashboard.html", data); err != nil {
		s.logger.Error("template render failed", "error", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
	}
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	// Use consecutive failures to avoid flapping on single check failures.
	// Unhealthy only after 3+ consecutive failures (matches broker_unreachable threshold).
	snap := s.buffer.SnapshotSince(time.Now().Add(-5 * time.Minute))
	latest := s.buffer.Latest()
	healthy := snap.ConsecutiveFailures < 3

	status := map[string]any{
		"healthy":              healthy,
		"time":                 time.Now().UTC(),
		"consecutive_failures": snap.ConsecutiveFailures,
	}

	if latest != nil {
		status["last_check"] = latest.Timestamp
		status["connected"] = latest.Connected
	}

	w.Header().Set("Content-Type", "application/json")
	if !healthy {
		w.WriteHeader(http.StatusServiceUnavailable)
	}
	json.NewEncoder(w).Encode(status)
}

func (s *Server) handleCurrentMetrics(w http.ResponseWriter, r *http.Request) {
	snap := s.buffer.SnapshotSince(time.Now().Add(-5 * time.Minute))
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(snap)
}

func (s *Server) handleMetricsHistory(w http.ResponseWriter, r *http.Request) {
	// Default to last hour, support ?window=24h
	window := 1 * time.Hour
	if windowParam := r.URL.Query().Get("window"); windowParam != "" {
		if d, err := time.ParseDuration(windowParam); err == nil {
			window = d
		}
	}

	samples := s.buffer.Since(time.Now().Add(-window))

	// Downsample for large windows to keep response size manageable
	maxPoints := 200
	if len(samples) > maxPoints {
		step := len(samples) / maxPoints
		downsampled := make([]metrics.Sample, 0, maxPoints)
		for i := 0; i < len(samples); i += step {
			downsampled = append(downsampled, samples[i])
		}
		samples = downsampled
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(samples)
}

func (s *Server) handleAlerts(w http.ResponseWriter, r *http.Request) {
	active := s.alertEngine.ActiveAlerts()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(active)
}
