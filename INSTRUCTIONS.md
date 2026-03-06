# hivemq-monitor — Build Instructions

## Overview

`hivemq-monitor` is a single Go binary that monitors HiveMQ Cloud managed broker clusters using an MQTT canary pattern. It connects as a regular MQTT client, publishes test messages, measures health, and alerts to Microsoft Teams. It also serves a built-in web dashboard.

This document is a comprehensive instruction set for building out the project from the existing scaffold. The scaffold has all the core files in place but needs the details fleshed out, tests written, and rough edges smoothed.

---

## Architecture

A single Go service deployed per environment into AKS via Flux. One instance monitors one HiveMQ Cloud cluster.

### Components

1. **MQTT Canary** (`internal/canary/`) — Connects to HiveMQ Cloud via TLS using Paho MQTT. Every 30 seconds it publishes a canary message, subscribes for it, and measures round-trip latency, delivery success, and connection health. Tests both QoS 0 and QoS 1.

2. **Metrics Ring Buffer** (`internal/metrics/`) — In-memory circular buffer holding 2880 samples (24h at 30s intervals). Computes aggregate snapshots: average, P95, max round-trip, delivery success rate, uptime percentage, consecutive failures, reconnect count.

3. **Alert Engine** (`internal/alerts/`) — Evaluates configurable threshold rules against metric snapshots. Sends Microsoft Teams Adaptive Cards via Power Automate Workflow webhooks. Implements a state machine: OK → FIRING → RESOLVED. Also sends an hourly status report digest.

4. **Azure Blob Storage** (`internal/blobstore/`) — Appends metric data as daily JSONL files to Azure Blob Storage append blobs. Buffered writes flushed every 60 seconds. Path format: `metrics/YYYY-MM-DD.jsonl`.

5. **Web Dashboard + API** (`internal/server/`) — Chi router serving Go HTML templates with Alpine.js and Chart.js. Auto-refreshes every 30s. JSON API at `/api/metrics/current`, `/api/metrics/history`, `/api/alerts`, `/api/health` for future MCP integration.

6. **Config** (`internal/config/`) — YAML config with `${ENV_VAR}` expansion for secrets. Loaded at startup, validated, with sensible defaults.

### Data Flow

```
HiveMQ Cloud Broker
        ↑ MQTT (TLS 8883)
        |
   ┌────┴────┐
   │  Canary  │ ← publishes test msgs, subscribes, measures round-trip
   └────┬────┘
        │ Sample
        ▼
   ┌──────────┐      ┌───────────┐      ┌──────────────┐
   │ Ring     │─────▶│ Alert     │─────▶│ Teams        │
   │ Buffer   │      │ Engine    │      │ Webhook      │
   └────┬─────┘      └───────────┘      └──────────────┘
        │                                     ▲
        │                                     │ Hourly report
        │                                     │ + threshold alerts
        ├──────▶ Blob Storage (daily JSONL)
        │
        └──────▶ Dashboard (Chi + Alpine.js + Chart.js)
                 + JSON API (/api/*)
```

---

## Technology Stack

- **Language**: Go 1.22+
- **MQTT Client**: `github.com/eclipse/paho.mqtt.golang` — standard Go MQTT client, supports MQTT 3.1.1/5.0, TLS, all QoS levels
- **HTTP Router**: `github.com/go-chi/chi/v5` — lightweight, idiomatic, stdlib-compatible
- **Config**: `gopkg.in/yaml.v3`
- **Azure Blob**: `github.com/Azure/azure-sdk-for-go/sdk/storage/azblob`
- **Azure Auth**: `github.com/Azure/azure-sdk-for-go/sdk/azidentity` (for managed identity support)
- **Frontend**: Go `html/template` + Alpine.js 3.x + Chart.js 4.x (loaded from CDN, no build step)
- **Structured Logging**: `log/slog` (stdlib)

---

## Project Structure

```
hivemq-monitor/
├── cmd/monitor/main.go            # Entrypoint, wires components, graceful shutdown
├── internal/
│   ├── config/config.go           # YAML config types, loading, env var expansion, validation
│   ├── canary/canary.go           # MQTT canary client, health check loop
│   ├── metrics/metrics.go         # Sample/Snapshot types, ring buffer, P95/avg computation
│   ├── blobstore/blobstore.go     # Azure Blob append blob writer (daily JSONL)
│   ├── alerts/engine.go           # Alert state machine, Teams Adaptive Card webhooks, hourly report
│   └── server/server.go           # Chi HTTP server, dashboard handler, JSON API
├── web/
│   ├── templates/dashboard.html   # Go template with Alpine.js + Chart.js
│   └── static/css/dashboard.css   # Dark theme dashboard styles
├── deploy/
│   └── k8s.yaml                   # Deployment + Service + ConfigMap (Flux-ready)
├── config.example.yaml            # Example config with all options documented
├── Dockerfile                     # Multi-stage build, ~15MB final image
└── go.mod
```

---

## Detailed Component Specifications

### 1. MQTT Canary (`internal/canary/`)

**Responsibilities:**
- Connect to HiveMQ Cloud broker using TLS with username/password auth
- Run canary checks on a configurable interval (default 30s)
- For each check, for each configured QoS level (default 0 and 1):
  - Generate a unique correlation ID (`{unix_nano}-{qos}`)
  - Subscribe to a response topic: `{topic_prefix}/{correlation_id}/response`
  - Publish the correlation ID as payload to that same topic
  - Wait for the message to arrive back (round-trip)
  - Record: connected status, connect duration, round-trip latency, sent/received booleans, errors, reconnect events
- Handle reconnects gracefully via Paho's auto-reconnect
- Track reconnect count for alerting

**Key Implementation Notes:**
- Use `ssl://` scheme for TLS connections to HiveMQ Cloud (port 8883)
- Set `CleanSession(true)` — we don't need persistent sessions for monitoring
- The `onConnect` handler must re-subscribe after reconnects
- Clean up pending message tracking to avoid memory leaks on timeouts
- Each `checkQoS()` call produces one `metrics.Sample` added to the ring buffer

**Edge Cases to Handle:**
- Broker completely unreachable (connection timeout)
- Connected but subscribe fails
- Connected, subscribed, published, but message never arrives (delivery timeout)
- Rapid reconnect storms (track but don't spam alerts)
- Stale pending messages after timeout (cleanup map)

### 2. Metrics (`internal/metrics/`)

**Types:**

`Sample` — raw result of a single canary check:
- `Timestamp`, `Connected`, `ConnectDurationMs`, `RoundTripMs`, `QoS`, `MessageSent`, `MessageReceived`, `Error`, `ReconnectOccurred`

`Snapshot` — aggregated summary over a time window:
- `Timestamp`, `Connected` (current), `ConsecutiveFailures`, `RoundTripAvgMs`, `RoundTripP95Ms`, `RoundTripMaxMs`, `DeliverySuccessRate`, `MessagesSent`, `MessagesReceived`, `ReconnectCount`, `UptimePercent`, `LastError`, `WindowDuration`

**Ring Buffer:**
- Fixed size (2880 = 24h at 30s), circular, thread-safe (`sync.RWMutex`)
- Methods: `Add(Sample)`, `All()`, `Since(time.Time)`, `Latest()`, `Count()`
- `SnapshotSince(time.Time)` computes aggregates from samples in the window
- P95 calculation: sort round-trip values, take the value at rank `0.95 * (n-1)` with linear interpolation
- `ConsecutiveFailures`: count backward from the most recent sample

### 3. Alert Engine (`internal/alerts/`)

**Alert Lifecycle (per rule):**

```
OK ──[threshold breached]──▶ FIRING ──[threshold recovered]──▶ OK
         │                       │                               │
    send alert card         no reminders                   send resolved card
                           (hourly report shows it)        (includes duration)
```

- On OK → FIRING: send one Teams Adaptive Card immediately
- While FIRING: silence. The hourly report includes "Active Alerts" section
- On FIRING → OK: send one resolved card with incident duration
- No reminder messages — the hourly report is the reminder

**Hourly Status Report:**
- Fires every hour regardless of health status (always send, even when green)
- Contains: connection status, uptime %, round-trip avg/P95/max, messages sent/received, delivery rate, reconnect count, active alerts section, last error if any
- Uses a single Adaptive Card FactSet for clean formatting

**Teams Webhook Format:**
- Must use Power Automate Workflow webhooks (NOT legacy O365 Connectors — those are being retired)
- Payload is an Adaptive Card wrapped in an attachments array:

```json
{
  "type": "message",
  "attachments": [{
    "contentType": "application/vnd.microsoft.card.adaptive",
    "content": {
      "$schema": "http://adaptivecards.io/schemas/adaptive-card.json",
      "type": "AdaptiveCard",
      "version": "1.4",
      "body": [
        { "type": "TextBlock", "text": "title", "size": "Large", "weight": "Bolder" },
        { "type": "FactSet", "facts": [{"title": "key", "value": "val"}] }
      ]
    }
  }]
}
```

- Rate limiting: Teams throttles at 4 requests/second. Implement retry with exponential backoff (3 attempts, 2s/4s/6s delays). Check for HTTP 429.
- Content-Type must be `application/json`

**Metric Extraction Map:**
- `"connection_failures"` → `snap.ConsecutiveFailures`
- `"round_trip_p95_ms"` → `snap.RoundTripP95Ms`
- `"round_trip_avg_ms"` → `snap.RoundTripAvgMs`
- `"delivery_success_rate"` → `snap.DeliverySuccessRate`
- `"reconnect_count"` → `snap.ReconnectCount`
- `"uptime_percent"` → `snap.UptimePercent`

**Operators:** `>`, `>=`, `<`, `<=`, `==`

### 4. Azure Blob Storage (`internal/blobstore/`)

**Design:**
- Daily append blobs at path `metrics/YYYY-MM-DD.jsonl`
- Each line is a JSON-serialized `Sample` or `Snapshot`
- Internal write buffer flushed every 60 seconds (batches writes, reduces API calls)
- Use Azure SDK `AppendBlobClient` — create blob if it doesn't exist (once per day), then `AppendBlock` for data
- Retention: configurable (default 90 days) — implement cleanup of old blobs on startup or as a periodic task
- Disabled by default in config (`enabled: false`)

**Azure SDK Integration (needs to be fleshed out):**
- The scaffold has TODO stubs for the actual Azure SDK calls
- Support both connection string auth (`account_name` + `account_key`) and managed identity (`azidentity.NewDefaultAzureCredential`)
- Handle the "blob doesn't exist yet" case on first append of each day: call `Create()` first, catch `BlobAlreadyExists` error and proceed

### 5. Web Dashboard + API (`internal/server/`)

**Dashboard (GET /):**
- Server-rendered Go template with data injected on first load
- Alpine.js manages client-side state and auto-refresh (every 30s via `fetch`)
- Chart.js renders two charts:
  - Round-trip latency time series (line chart, filled, 0.3 tension)
  - Message delivery status (stepped line, sent vs received)
- Status cards: connection (with color coding), P95 latency, delivery rate, uptime
- Active alerts banner (red, only shown when alerts are firing)
- Stats table with full snapshot data

**API Endpoints:**
- `GET /api/health` — returns `{"healthy": true/false, "connected": bool, "last_check": timestamp}`. Returns 503 if unhealthy. Used as K8s liveness/readiness probe.
- `GET /api/metrics/current` — returns current `Snapshot` (last 5 minutes)
- `GET /api/metrics/history?window=1h` — returns array of `Sample` objects. Supports duration param. Downsamples to max 200 points for large windows.
- `GET /api/alerts` — returns map of currently firing alert states

**Static Files:**
- Served from `web/static/` via `http.FileServer`
- Alpine.js and Chart.js loaded from CDN (no local copies needed)
- Dashboard CSS: dark theme (#0f172a background), responsive grid, Tailwind-inspired color palette

**Template Functions:**
- `formatTime(time.Time)` — returns "HH:MM:SS" UTC
- `formatDuration(time.Duration)` — human-readable duration
- `jsonMarshal(interface{})` — injects Go data as JS objects for Alpine.js initialization

### 6. Config (`internal/config/`)

**Environment Variable Expansion:**
- `${VAR_NAME}` syntax in YAML values, expanded via `os.Expand`
- If env var not set, leaves the `${VAR_NAME}` literal (useful for detecting misconfiguration)

**Validation:**
- Required: `broker.host`, `broker.username`, `broker.password`, `alerts.teams_webhook_url`
- Each alert rule must have `name` and `condition`
- Collect all errors and report them together

**Defaults:**
- `broker.port`: 8883
- `broker.client_id`: `"hivemq-monitor-{hostname}"`
- `canary.interval`: 30s
- `canary.timeout`: 10s
- `canary.topic_prefix`: `"monitor/canary"`
- `canary.qos_levels`: [0, 1]
- `alerts.hourly_report.enabled`: true
- `alerts.hourly_report.interval`: 1h
- `alerts.hourly_report.include_when_healthy`: true
- `server.port`: 8080
- `storage.azure_blob.retention_days`: 90
- Rule defaults: `operator` = `">"`, `severity` = `"warning"`

---

## Deployment

### Kubernetes

- Single `Deployment` with 1 replica (no need for multiple — it's a canary, not a load-balanced service)
- Config injected via `ConfigMap` mounted as `/app/config.yaml`
- Secrets via `Secret` referenced as env vars: `HIVEMQ_CANARY_PASSWORD`, `TEAMS_WEBHOOK_URL`, optionally `AZURE_STORAGE_ACCOUNT` and `AZURE_STORAGE_KEY`
- Resources: 50m CPU / 64Mi request, 200m CPU / 128Mi limit (this is a very lightweight service)
- Liveness probe: `GET /api/health` every 30s (initial delay 30s)
- Readiness probe: `GET /api/health` every 10s (initial delay 10s)
- Service: ClusterIP on port 8080 (dashboard accessible via port-forward or internal ingress)

### Docker

Multi-stage build:
1. `golang:1.22-alpine` builder stage — compile static binary
2. `alpine:3.19` runtime — ca-certificates + tzdata + binary + web assets
3. Final image ~15MB

### HiveMQ Cloud Setup

Create dedicated MQTT credentials for the canary user in the HiveMQ Cloud console:
- Username: `canary-monitor` (or similar)
- Permissions: publish and subscribe on `monitor/canary/#`
- This user should have minimal permissions — it only needs access to its own canary topics

### Teams Webhook Setup

1. In Microsoft Teams, go to the target monitoring channel
2. Click `...` → Manage channel → Connectors/Workflows
3. Create a new Workflow using the "When a Teams webhook request is received" template
4. Copy the generated webhook URL into the config as `TEAMS_WEBHOOK_URL`
5. This uses Power Automate Workflows (NOT the legacy O365 Connectors which are being retired by April 2026)

---

## Testing Strategy

### Unit Tests

- `metrics/metrics_test.go`: Ring buffer add/retrieve, snapshot computation, P95 calculation, edge cases (empty buffer, single sample, full buffer wraparound)
- `alerts/engine_test.go`: State machine transitions (OK→FIRING, FIRING→OK, staying in FIRING), metric extraction, threshold evaluation with different operators, Adaptive Card JSON structure validation
- `config/config_test.go`: Config loading, env var expansion, defaults, validation errors

### Integration Tests

- `canary/canary_test.go`: Use a mock MQTT broker (or HiveMQ CE in Docker) to test the full canary check cycle including subscribe, publish, round-trip measurement
- `alerts/webhook_test.go`: Use `httptest.Server` to mock the Teams webhook endpoint, verify Adaptive Card payloads, test retry on 429

### Manual Testing

- Run locally with `go run ./cmd/monitor --config config.yaml --log-level debug`
- Point at a real HiveMQ Cloud cluster or a local HiveMQ CE instance
- Use a test Teams channel for webhook validation
- Verify the dashboard loads at `http://localhost:8080`
- Verify API responses at `/api/health`, `/api/metrics/current`, etc.
- Simulate failures by temporarily breaking the broker connection and verify alerts fire

---

## Things to Build Out / Flesh Out

These are areas where the scaffold has the structure but needs real implementation:

1. **Azure Blob SDK integration** — Replace the TODO stubs in `blobstore.go` with actual `azblob` SDK calls. Support both connection string and managed identity auth. Handle daily blob creation and append operations.

2. **Embed static files** — The `server.go` uses `embed.FS` directives but needs the paths corrected for the final project layout. Currently falls back to disk reads. Make embed work properly for the production Docker image.

3. **go.sum** — Run `go mod tidy` after all imports are finalized to generate the lock file.

4. **Graceful MQTT disconnect** — Ensure the canary client disconnects cleanly on shutdown, unsubscribes from topics, and drains any pending checks.

5. **Alert engine evaluation interval** — Currently hardcoded to 35s in the engine. This should align with the canary interval or be independently configurable.

6. **Hourly report timing** — The first hourly report fires after a full hour. Consider sending an initial report shortly after startup (say 5 minutes) so the team gets immediate confirmation it's working.

7. **Dashboard template embedding** — Verify the `embed.FS` paths work correctly when built as a Docker image. The `//go:embed` directives in `server.go` reference relative paths that need to match the final binary's working directory.

8. **Health endpoint semantics** — Currently returns unhealthy if the latest sample failed. Consider a more nuanced approach: unhealthy only after N consecutive failures (matching the `broker_unreachable` alert threshold).

9. **Blob retention cleanup** — Implement a periodic task that deletes blobs older than `retention_days`.

10. **Structured error types** — Consider wrapping errors with context for better debugging in logs.

11. **Metrics for the monitor itself** — Optionally expose Prometheus metrics about the monitor's own operation (canary check duration, webhook send success/failure, etc.) for meta-monitoring.

---

## Config Reference

```yaml
broker:
  host: "your-cluster.s1.eu.hivemq.cloud"  # Required. HiveMQ Cloud hostname
  port: 8883                                 # Default: 8883
  tls: true                                  # Default: false
  username: "canary-monitor"                 # Required. MQTT username
  password: "${HIVEMQ_CANARY_PASSWORD}"      # Required. Supports ${ENV_VAR}
  client_id: "hivemq-monitor-prod"           # Default: "hivemq-monitor-{hostname}"

canary:
  interval: 30s        # How often to run checks. Default: 30s
  timeout: 10s         # Timeout per check operation. Default: 10s
  topic_prefix: "monitor/canary"  # MQTT topic prefix. Default: "monitor/canary"
  qos_levels: [0, 1]  # QoS levels to test. Default: [0, 1]

alerts:
  teams_webhook_url: "${TEAMS_WEBHOOK_URL}"  # Required. Power Automate Workflow URL

  hourly_report:
    enabled: true              # Default: true
    interval: 1h               # Default: 1h
    include_when_healthy: true  # Default: true (send even when everything is green)

  rules:
    - name: "broker_unreachable"       # Unique name for this rule
      condition: "connection_failures" # Metric to evaluate
      threshold: 3                     # Threshold value
      operator: ">"                    # >, >=, <, <=, ==. Default: ">"
      severity: "critical"             # critical or warning. Default: "warning"

    - name: "high_latency"
      condition: "round_trip_p95_ms"
      threshold: 500
      operator: ">"
      severity: "warning"

    - name: "message_loss"
      condition: "delivery_success_rate"
      threshold: 0.95
      operator: "<"
      severity: "critical"

    - name: "connection_instability"
      condition: "reconnect_count"
      threshold: 5
      operator: ">"
      severity: "warning"
      window: 10m   # Optional: evaluate within this time window

storage:
  azure_blob:
    enabled: false                          # Set true to enable
    container: "hivemq-metrics"             # Blob container name
    account_name: "${AZURE_STORAGE_ACCOUNT}"
    account_key: "${AZURE_STORAGE_KEY}"
    retention_days: 90                      # Default: 90

server:
  port: 8080  # Dashboard + API port. Default: 8080
```

---

## Available Metric Conditions for Alert Rules

| Condition | Type | Description |
|-----------|------|-------------|
| `connection_failures` | int | Consecutive failed canary checks (connection or delivery) |
| `round_trip_p95_ms` | float | 95th percentile round-trip latency in milliseconds |
| `round_trip_avg_ms` | float | Average round-trip latency in milliseconds |
| `delivery_success_rate` | float | Ratio of messages received / messages sent (0.0 to 1.0) |
| `reconnect_count` | int | Number of MQTT reconnects in the evaluation window |
| `uptime_percent` | float | Percentage of checks where broker was connected (0.0 to 100.0) |
