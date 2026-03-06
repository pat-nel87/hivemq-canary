# hivemq-canary

A lightweight Go service that monitors [HiveMQ Cloud](https://www.hivemq.com/mqtt-cloud-broker/) MQTT broker clusters using the canary pattern. It connects as a regular MQTT client, publishes test messages, measures round-trip latency and delivery success, and alerts to Microsoft Teams. Includes a built-in web dashboard and optional Azure Blob Storage for long-term metrics retention.

## Architecture

```
HiveMQ Cloud Broker
        ^ MQTT (TLS 8883)
        |
   +----+----+
   |  Canary  |  publishes test msgs, subscribes, measures round-trip
   +----+----+
        | Sample
        v
   +----------+      +-----------+      +--------------+
   | Ring      |----->| Alert     |----->| Teams        |
   | Buffer    |      | Engine    |      | Webhook      |
   +----+------+      +-----------+      +--------------+
        |                                     ^
        |                              Hourly report
        |                              + threshold alerts
        +------> Blob Storage (daily JSONL)
        |
        +------> Dashboard (Chi + Alpine.js + Chart.js)
                 + JSON API (/api/*)
```

One instance monitors one HiveMQ Cloud cluster. Deployed per environment into AKS via Flux GitOps.

### Components

| Component | Package | Description |
|-----------|---------|-------------|
| MQTT Canary | `internal/canary/` | Connects via TLS using Paho MQTT. Publishes canary messages every 30s, subscribes for round-trip, tests QoS 0 and QoS 1. |
| Metrics Ring Buffer | `internal/metrics/` | In-memory circular buffer (2880 samples = 24h). Computes avg, P95, max latency, delivery rate, uptime %, consecutive failures. |
| Alert Engine | `internal/alerts/` | Evaluates threshold rules against metric snapshots. Sends Teams Adaptive Cards via Power Automate Workflow webhooks. State machine: OK → FIRING → RESOLVED. Hourly status digest. |
| Blob Storage | `internal/blobstore/` | Appends metrics as daily JSONL to Azure Blob Storage append blobs. Buffered writes flushed every 60s. Configurable retention with automatic cleanup. |
| Web Dashboard + API | `internal/server/` | Chi router serving Go HTML templates with Alpine.js and Chart.js. Auto-refreshes every 30s. JSON API for programmatic access. |
| Config | `internal/config/` | YAML config with `${ENV_VAR}` expansion. Validated at startup with sensible defaults. |

## Getting Started

### Prerequisites

- Go 1.22+
- An MQTT broker (HiveMQ Cloud or local HiveMQ CE for testing)
- A Microsoft Teams channel with a Power Automate Workflow webhook (for alerts)
- (Optional) An Azure Storage Account (for blob storage metrics)

### Configuration

Copy the example config and fill in your values:

```sh
cp config.example.yaml config.yaml
```

Set required environment variables for secrets:

```sh
export HIVEMQ_CANARY_PASSWORD="your-mqtt-password"
export TEAMS_WEBHOOK_URL="https://prod-xx.westus.logic.azure.com/workflows/..."
```

For Azure Blob Storage with managed identity, set:

```sh
export AZURE_STORAGE_ACCOUNT="yourstorageaccount"
export AZURE_CLIENT_ID="your-managed-identity-client-id"  # for user-assigned managed identity
```

Or with shared key auth:

```sh
export AZURE_STORAGE_ACCOUNT="yourstorageaccount"
export AZURE_STORAGE_KEY="your-storage-account-key"
```

### Build and Run

```sh
go build -o hivemq-canary ./cmd/monitor
./hivemq-canary --config config.yaml --log-level info
```

The dashboard will be available at `http://localhost:8080`.

### Docker

```sh
docker build -t hivemq-canary .
docker run -p 8080:8080 \
  -e HIVEMQ_CANARY_PASSWORD=secret \
  -e TEAMS_WEBHOOK_URL=https://... \
  -v $(pwd)/config.yaml:/app/config.yaml \
  hivemq-canary
```

The image is a multi-stage build producing a ~15MB Alpine-based container with the binary and embedded web assets.

## Configuration Reference

```yaml
broker:
  host: "your-cluster.s1.eu.hivemq.cloud"  # Required
  port: 8883                                 # Default: 8883
  tls: true                                  # Default: false
  username: "canary-monitor"                 # Required
  password: "${HIVEMQ_CANARY_PASSWORD}"      # Required. Supports ${ENV_VAR}
  client_id: "hivemq-canary-prod"            # Default: "hivemq-canary-{hostname}"

canary:
  interval: 30s                    # Default: 30s
  timeout: 10s                     # Default: 10s
  topic_prefix: "monitor/canary"   # Default: "monitor/canary"
  qos_levels: [0, 1]              # Default: [0, 1]

alerts:
  teams_webhook_url: "${TEAMS_WEBHOOK_URL}"  # Required. Power Automate Workflow URL

  hourly_report:
    enabled: true               # Default: true
    interval: 1h                # Default: 1h
    include_when_healthy: true  # Default: true

  rules:
    - name: "broker_unreachable"
      condition: "connection_failures"
      threshold: 3
      operator: ">"             # >, >=, <, <=, ==. Default: ">"
      severity: "critical"      # critical or warning. Default: "warning"

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
      window: 10m               # Optional evaluation window

storage:
  azure_blob:
    enabled: false                            # Default: false
    container: "hivemq-metrics"
    account_name: "${AZURE_STORAGE_ACCOUNT}"
    account_key: "${AZURE_STORAGE_KEY}"       # Omit to use managed identity
    retention_days: 90                        # Default: 90

server:
  port: 8080  # Default: 8080
```

### Available Alert Conditions

| Condition | Type | Description |
|-----------|------|-------------|
| `connection_failures` | int | Consecutive failed canary checks |
| `round_trip_p95_ms` | float | 95th percentile round-trip latency (ms) |
| `round_trip_avg_ms` | float | Average round-trip latency (ms) |
| `delivery_success_rate` | float | Messages received / sent (0.0–1.0) |
| `reconnect_count` | int | MQTT reconnects in the evaluation window |
| `uptime_percent` | float | Connected checks percentage (0.0–100.0) |

## API Endpoints

| Endpoint | Description |
|----------|-------------|
| `GET /` | Web dashboard with live charts and status cards |
| `GET /api/health` | Health check (503 if 3+ consecutive failures). Used as K8s liveness/readiness probe. |
| `GET /api/metrics/current` | Current snapshot (last 5 minutes) |
| `GET /api/metrics/history?window=1h` | Sample history. Supports duration param. Downsamples to max 200 points. |
| `GET /api/alerts` | Currently firing alert states |

## Kubernetes Deployment

The `deploy/` directory contains ready-to-use Kubernetes manifests:

- `deploy/k8s.yaml` — Deployment, Service, and ConfigMap
- `deploy/kustomization.yaml` — Kustomize base
- `deploy/flux/` — Flux GitOps resources (GitRepository + Kustomization)

### Deploying with Flux

1. Create the Kubernetes secret with your credentials:

```sh
kubectl create secret generic hivemq-canary-secrets \
  --from-literal=canary-password='your-mqtt-password' \
  --from-literal=teams-webhook-url='https://prod-xx.westus.logic.azure.com/workflows/...' \
  --from-literal=azure-storage-account='yourstorageaccount' \
  --from-literal=azure-storage-key='your-key'  # omit if using managed identity
```

2. Update `deploy/flux/gitrepository.yaml` with your actual repository URL.

3. Update `deploy/k8s.yaml` with your container registry and broker details.

4. Apply the Flux resources:

```sh
kubectl apply -f deploy/flux/
```

### Resource Requirements

| | CPU | Memory |
|---|-----|--------|
| Request | 50m | 64Mi |
| Limit | 200m | 128Mi |

### Azure Managed Identity

When deployed to AKS, the service authenticates to Azure Blob Storage via `DefaultAzureCredential`, which supports:

- **Environment variables** — Set `AZURE_CLIENT_ID` on the pod for user-assigned managed identity
- **Workload Identity** — Add `azure.workload.identity/client-id` annotation to the service account
- **System-assigned managed identity** — No additional config needed

No `account_key` is required when using managed identity. Just set `account_name` and `enabled: true` in the storage config.

## Testing

```sh
# Run all tests
go test ./...

# Run tests for a specific package
go test ./internal/metrics/

# Run a single test
go test ./internal/alerts/ -run TestAlertStateTransition

# Run with verbose output
go test -v ./...

# Run with race detector
go test -race ./...
```

## Teams Webhook Setup

This project uses **Power Automate Workflow** webhooks (NOT legacy O365 Connectors, which are retired).

1. In Microsoft Teams, go to the target channel
2. Click **...** → **Manage channel** → **Connectors/Workflows**
3. Create a new Workflow using the **"When a Teams webhook request is received"** template
4. Copy the generated webhook URL into `TEAMS_WEBHOOK_URL`

Alerts are sent as Adaptive Cards with retry and exponential backoff for rate limiting (HTTP 429).

## HiveMQ Cloud Setup

Create dedicated MQTT credentials for the canary in the HiveMQ Cloud console:

- **Username**: `canary-monitor` (or similar)
- **Permissions**: Publish and subscribe on `monitor/canary/#`
- This user should have minimal permissions — it only needs access to its own canary topics

## License

Private — internal use only.
