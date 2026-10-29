# ── Build stage ──────────────────────────────────────────────────────────────
FROM golang:1.26-alpine AS build

WORKDIR /src

# Cache module downloads separately from source changes.
COPY go.mod go.sum ./
RUN go mod download

COPY . .
# Static binary: pgx is pure Go, so CGO can be off — no libc needed at runtime.
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags "-s -w" -o /server .

# ── Runtime stage ────────────────────────────────────────────────────────────
FROM alpine:3.21

# CA roots for outbound HTTPS (FCM, Resend, OSRM); tzdata for sane time
# handling; wget ships with busybox and is used by the compose healthcheck.
RUN apk add --no-cache ca-certificates tzdata \
    && addgroup -S app && adduser -S app -G app

COPY --from=build /server /server

USER app
EXPOSE 8080
ENTRYPOINT ["/server"]
