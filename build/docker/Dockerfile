# syntax=docker/dockerfile:1

# Build stage
FROM golang:1.26-alpine AS builder

RUN apk add --no-cache curl

WORKDIR /src

# Download Tailwind CSS standalone CLI
ARG TAILWIND_VERSION=v4.1.0
RUN curl -sLO https://github.com/nicolo-ribaudo/tailwind-cli-extra/releases/download/${TAILWIND_VERSION}/tailwindcss-extra-linux-x64 \
    && chmod +x tailwindcss-extra-linux-x64 \
    && mv tailwindcss-extra-linux-x64 /usr/local/bin/tailwindcss

# Cache Go modules
COPY go.mod go.sum ./
RUN go mod download

# Copy source
COPY . .

# Build Tailwind CSS
RUN tailwindcss -i web/static/css/input.css -o web/static/css/styles.css --minify

# Build Go binary
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /bin/stillwater ./cmd/stillwater

# Runtime stage
FROM alpine:3.21

RUN apk add --no-cache ca-certificates tzdata su-exec

# Create app user
RUN addgroup -g 1000 stillwater && \
    adduser -u 1000 -G stillwater -s /bin/sh -D stillwater

# Create data and music directories
RUN mkdir -p /data /music && chown stillwater:stillwater /data /music

COPY --from=builder /bin/stillwater /usr/local/bin/stillwater
COPY --from=builder /src/web/static /app/web/static
COPY build/docker/entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh

EXPOSE 8080

VOLUME ["/data", "/music"]

ENTRYPOINT ["/entrypoint.sh"]
CMD ["stillwater"]
