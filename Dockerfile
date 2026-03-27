FROM golang:1.22-alpine AS builder

RUN apk add --no-cache git ca-certificates

WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -ldflags="-s -w -X main.version=docker" \
    -trimpath -o gateway .

# --- Runtime ---
FROM alpine:3.20

RUN apk add --no-cache ca-certificates tzdata curl && \
    adduser -D -h /app gateway

WORKDIR /app
COPY --from=builder /build/gateway .

USER gateway

EXPOSE 8080

HEALTHCHECK --interval=30s --timeout=5s --start-period=5s --retries=3 \
    CMD curl -sf http://localhost:8080/health || exit 1

ENTRYPOINT ["./gateway"]
