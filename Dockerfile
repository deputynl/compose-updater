FROM golang:1.25-alpine AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -o /out/compose-updater .

FROM alpine:3.20

LABEL org.opencontainers.image.source="https://github.com/deputynl/compose-updater"
LABEL org.opencontainers.image.description="A tiny self-hosted web UI for batch-updating Docker Compose stacks"
LABEL org.opencontainers.image.licenses="MIT"

RUN apk add --no-cache docker-cli docker-cli-compose

WORKDIR /app
COPY --from=builder /out/compose-updater ./compose-updater
COPY templates ./templates
COPY static ./static

ENV COMPOSE_ROOT=/compose \
    DATA_DIR=/data \
    LISTEN_ADDR=:8080 \
    MAX_PARALLEL=4

VOLUME ["/data"]
EXPOSE 8080
ENTRYPOINT ["./compose-updater"]
