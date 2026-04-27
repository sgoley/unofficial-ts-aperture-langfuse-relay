# Aperture -> Langfuse Relay (Custom Go Adapter)

This repository now contains a compact Go service that runs inside your tailnet and translates Aperture webhook payloads into Langfuse ingestion events.

## Deployment Paths (Choose One)

1. Private tailnet node via embedded `tsnet` + Docker Compose (recommended)
2. Private tailnet node via embedded `tsnet` + host process manager (systemd/supervisord)
3. Public webhook via host `tailscaled` + Tailscale Funnel (`TSNET_ENABLED=false`)

If you are not sure, choose option 1.

## What It Does

- Accepts Aperture webhook calls on `POST /hooks/aperture`
- Optionally validates webhook authentication (`Authorization: Bearer <APERTURE_API_KEY>`)
- Maps each Aperture request into:
  - one `trace-create` event
  - one `generation-create` event
- Preserves full request and response context so Langfuse traces remain useful for debugging
- Queues events locally, returns `202`, then forwards to Langfuse via `POST /api/public/ingestion`
- Retries transient Langfuse failures (`429`/`5xx`) with bounded backoff
- Runs privately in your tailnet using `tsnet`

## Quick Start

1. Copy envs and set your keys:

```bash
cp .env.example .env
```

2. Export environment variables (or use your own process manager):

```bash
set -a
source .env
set +a
```

3. Run the relay:

```bash
go run .
```

When `TSNET_ENABLED=true`, it starts as a tailnet node and listens on `http://<TSNET_HOSTNAME>:8080/hooks/aperture`. Set `TSNET_TLS_ENABLED=true` to use a tailnet HTTPS listener instead.

For local development, the default `TSNET_STATE_DIR=./.tsnet` is fine. For long-running service deployments, use a persistent absolute path.

## Deploy in Docker (Recommended)

Yes, this is easy to deploy via Docker. This repo now includes a multi-stage `Dockerfile` and a `docker-compose.yml`.

1. Set `.env` values, especially:

- `LANGFUSE_PUBLIC_KEY`
- `LANGFUSE_SECRET_KEY`
- `APERTURE_API_KEY`
- `TS_AUTHKEY` (recommended for headless first boot)

2. Start with Compose:

```bash
docker compose up -d --build
```

3. Confirm logs:

```bash
docker compose logs -f
```

This stores tsnet identity in a persistent volume (`tsnet-state`) so the node stays registered across restarts.

Compose explicitly overrides `TSNET_STATE_DIR` to `/var/lib/tsnet` so the mounted volume is actually used.

Health endpoints:

- `/healthz`: process is alive
- `/readyz`: relay is usable; in tsnet mode this requires the node backend to be running

## Docker Run Example

```bash
docker build -t aperture-langfuse-relay:local .

docker run -d \
  --name aperture-langfuse-relay \
  --restart unless-stopped \
  --env-file .env \
  -e TSNET_STATE_DIR=/var/lib/tsnet \
  -v tsnet-state:/var/lib/tsnet \
  aperture-langfuse-relay:local
```

## Hosting in a Tailnet (How It Works)

- The app uses `tsnet` (embedded Tailscale client), so it joins your tailnet directly.
- You do not need to expose this service publicly to the internet.
- Aperture can call it over MagicDNS name: `http://<TSNET_HOSTNAME>:8080/hooks/aperture`.
- For first-time unattended deploys (server/container), use `TS_AUTHKEY`.
- Keep `TSNET_STATE_DIR` persisted (volume or host path) so identity survives restarts.
- `8080` is just the relay's local app port. Aperture does not require a special webhook port.
- Run one replica per `TSNET_HOSTNAME`/state directory. For multiple replicas, give each pod/process a unique hostname and persistent state.

## Non-Docker Hosting Options

- VM/bare metal process manager (systemd, supervisord, etc.)
  - `go build` once, run binary with env vars.
  - Example unit file: `deploy/systemd/aperture-langfuse-relay.service`
- Kubernetes
  - Run as a Deployment with a PersistentVolume for `/var/lib/tsnet`.
  - Supply env vars via Secret/ConfigMap.

In all cases, the key requirement is the same: persistent tsnet state + valid auth bootstrap.

## Alternative: Run as a Host Service + Tailscale Funnel

Yes, this is another possible solution.

Use this model when you want to run the relay as a normal host service (not embedded `tsnet`) and expose it through a Funnel URL.

### When to choose this

- You already run `tailscaled` on the host
- You want a public HTTPS endpoint without adding a separate reverse proxy
- You explicitly accept that Funnel is internet-accessible (protected by `APERTURE_API_KEY`)

### Funnel prerequisites

- MagicDNS enabled on the tailnet
- HTTPS certificates enabled for the tailnet
- Funnel enabled in tailnet policy / node attributes for the host machine
- Host `tailscaled` already authenticated and online

### Steps

1. Run relay in host-service mode (disable embedded tsnet):

```bash
TSNET_ENABLED=false \
LISTEN_ADDR=127.0.0.1:8080 \
go run .
```

2. Make sure host Tailscale is up:

```bash
tailscale status
```

If Funnel has not been enabled on the tailnet yet, run:

```bash
tailscale funnel
```

3. Serve the local relay over tailnet HTTPS, then enable Funnel:

```bash
tailscale serve --bg --https=443 http://127.0.0.1:8080
tailscale funnel --bg 443
```

4. Check Funnel URL/status:

```bash
tailscale funnel status
```

5. Use that HTTPS URL in Aperture hook config, for example:

```json
"hooks": {
  "langfuse-relay": {
    "url": "https://your-device.your-tailnet.ts.net/hooks/aperture",
    "apikey": "<same-value-as-APERTURE_API_KEY>",
    "authorization": "bearer",
    "timeout": "10s"
  }
}
```

### Security note

- `tsnet` mode: private tailnet-only endpoint (preferred default)
- Funnel mode: public internet endpoint through Tailscale relays; `APERTURE_API_KEY` is the only application-layer barrier

If you use Funnel, keep `APERTURE_API_KEY` enabled and rotate it regularly.

## Example systemd Deployment

1. Build the binary and install it:

```bash
go build -o aperture-langfuse-relay .
sudo install -m 0755 aperture-langfuse-relay /usr/local/bin/aperture-langfuse-relay
sudo useradd --system --home /var/lib/aperture-langfuse-relay --shell /usr/sbin/nologin aperture-langfuse-relay
sudo mkdir -p /opt/aperture-langfuse-relay /etc/aperture-langfuse-relay /var/lib/aperture-langfuse-relay/tsnet
sudo chown -R aperture-langfuse-relay:aperture-langfuse-relay /var/lib/aperture-langfuse-relay
```

2. Create `/etc/aperture-langfuse-relay/env` with values from `.env.example`, but set:

```dotenv
TSNET_STATE_DIR=/var/lib/aperture-langfuse-relay/tsnet
```

3. Install the unit file from `deploy/systemd/aperture-langfuse-relay.service` and start it:

```bash
sudo cp deploy/systemd/aperture-langfuse-relay.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now aperture-langfuse-relay
sudo systemctl status aperture-langfuse-relay
```

## Aperture Configuration Example

Add a hook in Aperture settings:

```json
"hooks": {
  "langfuse-relay": {
    "url": "http://aperture-langfuse-relay:8080/hooks/aperture",
    "apikey": "<same-value-as-APERTURE_API_KEY>",
    "authorization": "bearer",
    "timeout": "10s"
  }
}
```

Reference it in your grant capability:

```json
"send_hooks": [
  {
    "name": "langfuse-relay",
    "events": ["entire_request"],
    "send": [
      "request_body",
      "user_message",
      "response_body",
      "raw_responses",
      "estimated_cost",
      "tools"
    ]
  }
]
```

## Environment Variables

- `TSNET_ENABLED`: `true` to run inside tailnet using tsnet
- `TSNET_TLS_ENABLED`: `true` to use tsnet's HTTPS listener
- `TSNET_HOSTNAME`: node name inside the tailnet
- `TSNET_STATE_DIR`: local state directory for tsnet identity
- `TS_AUTHKEY`: optional auth key for non-interactive first boot
- `LISTEN_ADDR`: bind address, default `:8080`
- `WEBHOOK_PATH`: webhook endpoint path, default `/hooks/aperture`
- `APERTURE_API_KEY`: optional shared secret for incoming `Authorization: Bearer` webhooks
- `LANGFUSE_BASE_URL`: Langfuse host, default `https://cloud.langfuse.com`
- `LANGFUSE_PUBLIC_KEY`: required
- `LANGFUSE_SECRET_KEY`: required
- `LANGFUSE_ENV`: event environment tag
- `REQUEST_TIMEOUT`: per-attempt Langfuse request timeout, default `5s`
- `MAX_REQUEST_BODY_BYTES`: maximum inbound Aperture payload, default `3145728`
- `MAX_LANGFUSE_BATCH_BYTES`: maximum generated Langfuse ingestion body, default `3145728`
- `QUEUE_SIZE`: bounded in-memory queue depth, default `100`
- `WORKER_COUNT`: async forwarding workers, default `2`
- `RETRY_MAX_ATTEMPTS`: transient upstream retry attempts, default `3`
- `RETRY_BASE_DELAY`: first retry delay before jitter/backoff, default `250ms`
- `RETRY_MAX_DELAY`: maximum retry delay, default `5s`

## Operational Limits

- The queue is in-memory. If the process exits before workers drain, queued events are lost.
- If the queue is full, the relay returns `503` so Aperture can retry.
- Horizontal scaling requires unique tsnet hostnames and state directories per replica.

## Notes

- Langfuse labels `/api/public/ingestion` as legacy but still supported.
- If you prefer, this service can be upgraded later to emit OpenTelemetry traces to `/api/public/otel/v1/traces`.
- Health endpoint: `GET /healthz`
- The relay keeps Aperture `request_id` in correlation logic when present, but safely generates non-colliding IDs when it is absent.
