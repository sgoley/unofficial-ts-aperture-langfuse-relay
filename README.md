# Aperture -> Langfuse Relay (Custom Go Adapter)

This repository now contains a compact Go service that runs inside your tailnet and translates Aperture webhook payloads into Langfuse ingestion events.

## Deployment Paths (Choose One)

1. Private tailnet node via embedded `tsnet` + Docker Compose (recommended)
2. Private tailnet node via embedded `tsnet` + host process manager (systemd/supervisord)
3. Public webhook via host `tailscaled` + Tailscale Funnel (`TSNET_ENABLED=false`)

If you are not sure, choose option 1.

## What It Does

- Accepts Aperture webhook calls on `POST /hooks/aperture`
- Optionally validates webhook authentication (`bearer`, `x-api-key`, `x-goog-api-key`)
- Maps each Aperture request into:
  - one `trace-create` event
  - one `generation-create` event
- Sends these to Langfuse via `POST /api/public/ingestion`
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

When `TSNET_ENABLED=true`, it starts as a tailnet node and listens on `http://<TSNET_HOSTNAME>:8080/hooks/aperture`.

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

## Docker Run Example

```bash
docker build -t aperture-langfuse-relay:local .

docker run -d \
  --name aperture-langfuse-relay \
  --restart unless-stopped \
  --env-file .env \
  -v tsnet-state:/var/lib/tsnet \
  aperture-langfuse-relay:local
```

## Hosting in a Tailnet (How It Works)

- The app uses `tsnet` (embedded Tailscale client), so it joins your tailnet directly.
- You do not need to expose this service publicly to the internet.
- Aperture can call it over MagicDNS name: `http://<TSNET_HOSTNAME>:8080/hooks/aperture`.
- For first-time unattended deploys (server/container), use `TS_AUTHKEY`.
- Keep `TSNET_STATE_DIR` persisted (volume or host path) so identity survives restarts.

## Non-Docker Hosting Options

- VM/bare metal process manager (systemd, supervisord, etc.)
  - `go build` once, run binary with env vars.
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

3. Enable Funnel to forward public HTTPS to local relay port:

```bash
tailscale funnel 8080
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
- Funnel mode: public internet endpoint through Tailscale relays

If you use Funnel, keep `APERTURE_API_KEY` enabled and rotate it regularly.

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
- `TSNET_HOSTNAME`: node name inside the tailnet
- `TSNET_STATE_DIR`: local state directory for tsnet identity
- `TS_AUTHKEY`: optional auth key for non-interactive first boot
- `LISTEN_ADDR`: bind address, default `:8080`
- `WEBHOOK_PATH`: webhook endpoint path, default `/hooks/aperture`
- `APERTURE_API_KEY`: optional shared secret for incoming webhooks
- `LANGFUSE_BASE_URL`: Langfuse host, default `https://cloud.langfuse.com`
- `LANGFUSE_PUBLIC_KEY`: required
- `LANGFUSE_SECRET_KEY`: required
- `LANGFUSE_ENV`: event environment tag

## Notes

- Langfuse labels `/api/public/ingestion` as legacy but still supported.
- If you prefer, this service can be upgraded later to emit OpenTelemetry traces to `/api/public/otel/v1/traces`.
- Health endpoint: `GET /healthz`
