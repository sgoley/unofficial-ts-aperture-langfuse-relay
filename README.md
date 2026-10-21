# Aperture / OpenRouter → Langfuse Relay

This Go service can translate Aperture webhooks into Langfuse events or accept OpenRouter's native Langfuse Broadcast requests. Langfuse itself can stay private. The repository, binary, and Docker service retain their original names for existing deployments.

## Choose the source

| `RELAY_MODE` | Enabled endpoints | Incoming authentication |
| --- | --- | --- |
| `aperture` (default) | `POST /hooks/aperture` (or `WEBHOOK_PATH`) | Bearer `APERTURE_API_KEY`; optional for backward compatibility, strongly recommended |
| `openrouter` | `POST /api/public/otel/v1/traces`, `POST /api/public/ingestion`, `GET /api/public/projects` | Required Basic auth using `OPENROUTER_RELAY_PUBLIC_KEY` + `OPENROUTER_RELAY_SECRET_KEY` |
| `both` | Both sets of endpoints | Both sets of credentials are required |

All modes also serve `/healthz` and `/readyz`. Other paths, including the Langfuse UI, signup, admin APIs, and trace-reading APIs, return 404. OpenRouter routes are disabled in the default Aperture mode. In OpenRouter mode, the Aperture webhook is disabled.

The relay uses a fixed `LANGFUSE_BASE_URL` and its own private Langfuse **project** keys for outbound requests. Never use organization or instance-admin keys as upstream credentials. It never accepts a caller-supplied upstream URL.

## OpenRouter Broadcast setup

OpenRouter's [Langfuse Broadcast destination](https://openrouter.ai/docs/guides/features/broadcast/langfuse) already emits OpenTelemetry data, so the relay forwards the original payload unchanged. It supports HTTP JSON and protobuf, including gzip. The legacy Langfuse batch ingestion endpoint is also available for older senders. There is no LLM request routing through this service.

1. Create a Langfuse project and API key pair. Put those keys in `LANGFUSE_PUBLIC_KEY` / `LANGFUSE_SECRET_KEY`; set `LANGFUSE_BASE_URL` to the private Langfuse web origin (for example, `http://langfuse-web:3000` when both containers share a Docker network).
2. Set `RELAY_MODE=openrouter`, or `both` if you also receive Aperture hooks.
3. Generate independent relay credentials for `OPENROUTER_RELAY_PUBLIC_KEY` / `OPENROUTER_RELAY_SECRET_KEY`. For example, generate random values locally with `openssl rand -hex 32` and prefix them with `pk-lf-` and `sk-lf-` respectively. These credentials belong to the relay and need not exist in Langfuse. Keep them out of Git and logs.
4. Provide a public HTTPS origin for **the relay**, such as `https://trace-ingest.example.net`. Use a TLS reverse proxy or Funnel to the relay listener; Langfuse's own origin stays private. A Tailscale auth key joins the relay to your tailnet but does not make it reachable by OpenRouter's cloud service.
5. In the target OpenRouter workspace, open **Settings → Observability**, enable Broadcast, and configure a **Langfuse** destination. Set **Base URL** to the relay's public HTTPS origin without an `/api` suffix. Set **Public Key** and **Secret Key** to the `OPENROUTER_RELAY_*` credentials, rather than the private upstream Langfuse credentials.
6. Use **Test Connection**, save, then send a request through an allowed OpenRouter API key and confirm the trace appears in the intended Langfuse project. The relay permits authenticated `GET /api/public/projects` as a credential-check endpoint; the current OpenRouter docs do not specify exactly how its connection test is implemented. If it uses another route, inspect method/path/status logs before adding any narrow exception.
7. Choose Privacy Mode, workspace/key filters, and sampling in OpenRouter. Configure evaluation rules separately in Langfuse; this relay does not run evaluators.

OpenRouter forwarding waits for Langfuse's response and returns its status, body, and `Retry-After` to the sender, including OTLP partial-success responses and legacy 207 errors. It does not queue, retry, or return an early success for these requests. The sender decides whether to retry. Payloads, timestamps, identifiers, and evaluation events in legacy batches are not rewritten.

### Expose only the relay

The default Compose file publishes no host ports. To use a host TLS proxy or host Tailscale Funnel, create a local override:

```yaml
# docker-compose.public.yml
services:
  aperture-langfuse-relay:
    environment:
      RELAY_MODE: openrouter
      TSNET_ENABLED: "false"
    ports:
      - "127.0.0.1:4319:8080"
```

Start it with:

```bash
docker compose -f docker-compose.yml -f docker-compose.public.yml up -d --build
```

Ensure the container can reach the private upstream URL: attach it to Langfuse's Docker network in the override when using a service name such as `langfuse-web`. For a relay on a separate tailnet device, set `TSNET_ENABLED=true` and `LANGFUSE_USE_TSNET=true` to dial the private Langfuse origin through the relay's own embedded Tailscale identity. This requires `TS_AUTHKEY` or an existing persisted node identity, an upstream address reachable over Tailscale, and a tailnet grant allowing the relay to reach the upstream port. With `LANGFUSE_USE_TSNET=false` (default), outbound requests use ordinary container/host networking. Private HTTPS upstreams must have a certificate the relay trusts; do not disable certificate verification. Neither setting makes the inbound relay public: OpenRouter still needs a public proxy or Funnel to reach it.

After reviewing the existing Serve/Funnel configuration and confirming that HTTPS listener port 443 is free, a host Funnel can publish the loopback relay:

```bash
tailscale funnel status
tailscale funnel --bg 4319
tailscale funnel status
```

Funnel requires MagicDNS, HTTPS certificates, and tailnet Funnel permission. Use the displayed public origin in OpenRouter. This exposes the relay's allowed routes, not the Langfuse UI. It is still a public URL: the relay credentials provide access control. Stop this listener with `tailscale funnel --https=443 off`; do not reset other existing Serve/Funnel listeners.

If using `RELAY_MODE=both`, a direct Funnel also exposes the authenticated Aperture hook. Use a public proxy that only forwards the three OpenRouter routes, or run separate instances for private Aperture and public OpenRouter traffic. Only attach the optional relay ingress to public exposure; do not forward an entire shared Caddy listener.

The relay bounds request headers, request time, wire and decoded gzip body size, response size, and concurrent upstream requests. Redirects are refused, and client cookies, arbitrary headers, and incoming relay credentials are not forwarded to Langfuse. Add rate limiting at the public proxy for request-rate or failed-login limits; the relay's concurrency cap is not a per-IP rate limiter. URLs can be discovered through public DNS/certificate records even when they are obscure.

## Validation

```bash
go test -race ./...
go vet ./...
```

The tests use a local fake upstream and cover both source modes, authentication isolation, OTLP JSON/protobuf/gzip payload preservation, legacy batches, blocked routes, upstream errors, payload limits, redirect rejection, and cancellation/concurrency. They do not replace the live OpenRouter destination connection test.

## Aperture Deployment Paths (Choose One)

1. Private tailnet node via embedded `tsnet` + Docker Compose (recommended)
2. Private tailnet node via embedded `tsnet` + host process manager (systemd/supervisord)
3. Public webhook via host `tailscaled` + Tailscale Funnel (`TSNET_ENABLED=false`)

If you are not sure, choose option 1.

## Aperture forwarding

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

- The app uses `tsnet` (embedded Tailscale client), so it joins your tailnet directly for inbound connections. Set `LANGFUSE_USE_TSNET=true` to use that same node identity for outbound Langfuse HTTP requests; otherwise they use ordinary container/host networking.
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

3. Review existing Serve/Funnel listeners, then publish the loopback relay on a free HTTPS listener:

```bash
tailscale funnel status
tailscale funnel --bg 8080
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

That `send_hooks` entry is only part of the setup: the hook must also be included in a GRANT policy under `app.tailscale.com/cap/aperture`, with `src` scoped to the users/requestors who are allowed to activate it.

## Environment Variables

- `RELAY_MODE`: `aperture` (default), `openrouter`, or `both`
- `OPENROUTER_RELAY_PUBLIC_KEY`, `OPENROUTER_RELAY_SECRET_KEY`: required Basic auth credentials supplied to OpenRouter's Langfuse destination when OpenRouter mode is enabled; separate from the actual upstream project keys
- `OPENROUTER_MAX_IN_FLIGHT`: simultaneous OpenRouter upstream requests, default `8`; excess requests receive `503` with `Retry-After: 1`
- `TSNET_ENABLED`: `true` to run inside tailnet using tsnet
- `TSNET_TLS_ENABLED`: `true` to use tsnet's HTTPS listener
- `TSNET_HOSTNAME`: node name inside the tailnet
- `TSNET_STATE_DIR`: local state directory for tsnet identity
- `TS_AUTHKEY`: optional auth key for non-interactive first boot
- `LISTEN_ADDR`: bind address, default `:8080`
- `HEALTH_LISTEN_ADDR`: separate local/container health listener, default `:8081`; do not publish it
- `WEBHOOK_PATH`: webhook endpoint path, default `/hooks/aperture`
- `APERTURE_API_KEY`: shared secret for incoming `Authorization: Bearer` webhooks; optional in Aperture-only mode for compatibility, required in `both` mode
- `LANGFUSE_BASE_URL`: Langfuse host, default `https://cloud.langfuse.com`
- `LANGFUSE_USE_TSNET`: use the embedded node to dial the Langfuse origin, default `false`; requires `TSNET_ENABLED=true` and tailnet network permissions
- `LANGFUSE_PUBLIC_KEY`: required
- `LANGFUSE_SECRET_KEY`: required
- `LANGFUSE_ENV`: event environment tag
- `REQUEST_TIMEOUT`: per-attempt Langfuse request timeout, default `5s`
- `SHUTDOWN_TIMEOUT`: graceful shutdown timeout for HTTP server and workers, default `10s`
- `MAX_REQUEST_BODY_BYTES`: maximum inbound payload, default `3145728`; OpenRouter also enforces this limit on decoded gzip data
- `MAX_LANGFUSE_BATCH_BYTES`: maximum generated Aperture-to-Langfuse ingestion body, default `3145728`
- `QUEUE_SIZE`: bounded in-memory Aperture queue depth, default `100`
- `WORKER_COUNT`: async Aperture forwarding workers, default `2`
- `RETRY_MAX_ATTEMPTS`: transient upstream retry attempts, default `3`
- `RETRY_BASE_DELAY`: first retry delay before jitter/backoff, default `250ms`
- `RETRY_MAX_DELAY`: maximum retry delay, default `5s`

## Operational Limits

- The Aperture queue is in-memory. If the process exits before workers drain, queued events are lost. A `202` is not a durable-storage acknowledgement.
- If the Aperture queue is full, the relay returns `503` so Aperture can retry.
- OpenRouter forwarding is synchronous with a bounded concurrency cap; it does not use the Aperture queue or retry settings. Upstream response bodies are capped at 1 MiB; larger responses return 502.
- Both source modes target the same upstream project. Run separate instances if they should target different projects.
- If Aperture routes requests through OpenRouter and both emit traces for the same requests, you may get duplicate records. Choose one emitter for those requests or scope destinations accordingly.
- Horizontal scaling requires unique tsnet hostnames and state directories per replica.

## Notes

- The Aperture adapter uses legacy `trace-create` / `generation-create` ingestion events, supported by self-hosted Langfuse v3. Check compatibility before pointing it at Langfuse v4 or newer Cloud ingestion; the [API migration notes](https://langfuse.com/docs/api-and-data-platform/features/public-api) describe the legacy endpoint changes.
- OpenRouter mode already forwards OpenTelemetry traces to `/api/public/otel/v1/traces`. Migrating the Aperture adapter itself to emit OTLP remains separate work.
- Health endpoint: `GET /healthz`
- The relay keeps Aperture `request_id` in correlation logic when present, but safely generates non-colliding IDs when it is absent.
