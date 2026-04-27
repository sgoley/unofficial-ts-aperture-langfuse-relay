FROM golang:1.25-alpine AS builder
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o /out/aperture-langfuse-relay .

FROM alpine:3.21
RUN apk add --no-cache ca-certificates
RUN addgroup -S relay && adduser -S -G relay relay && mkdir -p /var/lib/tsnet && chown -R relay:relay /var/lib/tsnet
WORKDIR /app

COPY --from=builder /out/aperture-langfuse-relay /usr/local/bin/aperture-langfuse-relay

ENV TSNET_ENABLED=true
ENV TSNET_TLS_ENABLED=false
ENV TSNET_HOSTNAME=aperture-langfuse-relay
ENV TSNET_STATE_DIR=/var/lib/tsnet
ENV LISTEN_ADDR=:8080
ENV WEBHOOK_PATH=/hooks/aperture

VOLUME ["/var/lib/tsnet"]
USER relay

ENTRYPOINT ["/usr/local/bin/aperture-langfuse-relay"]
