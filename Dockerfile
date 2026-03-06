FROM golang:1.24-alpine AS builder

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /hivemq-canary ./cmd/monitor

FROM alpine:3.19
RUN apk --no-cache add ca-certificates tzdata
COPY --from=builder /hivemq-canary /usr/local/bin/hivemq-canary

WORKDIR /app
EXPOSE 8080

ENTRYPOINT ["hivemq-canary"]
CMD ["--config", "/app/config.yaml"]
