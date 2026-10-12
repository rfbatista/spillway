<p align="center">
  <img src="assets/logo.svg" alt="spillway" width="420">
</p>

<p align="center">
  A rate-limit-aware reverse proxy for LLM backends — Azure OpenAI, OpenAI, Anthropic, AWS Bedrock, OpenRouter and friends.
</p>

---

**spillway** sits between your services and one or more LLM providers. It picks
a healthy backend for each request based on live rate-limit headers, queues and
retries when every backend is saturated, translates OpenAI-shaped requests into
each provider's native API, and records every request's lifecycle for
Grafana/Loki. The backend list hot-reloads from AWS AppConfig without a restart.

## Features

- **Rate-limit-aware routing.** Each backend tracks remaining capacity from the
  `X-RateLimit-*` response headers. Saturated backends are skipped; with nowhere
  to go, the request waits in a per-backend queue and retries as capacity frees
  up.
- **Alias routing.** Send `X-Request-Model: <alias>` to target a group of
  backends; without it the pool round-robins across all of them.
- **Request mapping.** Clients speak the OpenAI Chat Completions API. The
  `openai`, `anthropic`, `bedrock` (Converse), `grok` and `passthrough` mappers
  inject the backend's model and translate to and from the provider's shape,
  including streaming and `reasoning_effort` → native thinking budgets.
- **Native Bedrock Converse.** Bodies already in Converse shape (as the AWS SDK
  sends them) are forwarded untouched, signed with SigV4 or a Bedrock API key.
- **Request tracking.** Every request and each upstream attempt is logged as
  structured `slog` JSON, with configurable body cropping/dropping so the
  interesting fields fit in the log.
- **Live dashboards.** Built-in pages stream quota and request events over SSE.
- **Hot reload.** Config comes from `PROXY_CONFIG` or the AWS AppConfig Agent,
  polled for changes and applied in place.

## How it works

```
client ──► Handler ──► Pool ──► Backend (mapper + transport) ──► provider
                        │  pick by alias, skip rate-limited
                        └─► Queue ──► RetryTransport (wait for capacity, retry)
```

| Component        | Role                                                                      |
| ---------------- | ------------------------------------------------------------------------- |
| `Pool`           | Holds the backends; round-robin selection, alias groups, liveness.        |
| `Backend`        | One upstream: URL, mapper, auth, usage window from rate-limit headers.    |
| `Queue`          | Per-backend buffer for requests that arrive while it is saturated.        |
| `RetryTransport` | Retries queued requests as capacity returns.                              |
| `RequestTracker` | Records the request lifecycle and publishes it to logs and the dashboard. |

All of it lives in `internal/proxy`.

## Quick start

Requires Go 1.24+ and `jq`. Write a config file (see [Configuration](#configuration)),
then:

```bash
PORT=3000 PROXY_CONFIG="$(jq -c . config.json)" go run ./cmd/server
```

Then point any OpenAI client at the proxy:

```bash
curl http://localhost:3000/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -H 'X-Request-Model: claude-opus' \
  -d '{"messages":[{"role":"user","content":"Hello"}]}'
```

For local work without real providers, run one of the fake backends in another
terminal and point a backend at it:

```bash
go run ./cmd/echoserver        # echoes requests back
go run ./cmd/openaiserver      # fake OpenAI
go run ./cmd/anthropicserver   # fake Anthropic (:8092)
go run ./cmd/bedrockserver     # fake Bedrock (:8093)
```

## Configuration

The proxy reads a JSON document with a `backends` array:

```json
{
  "backends": [
    {
      "name": "bedrock-opus",
      "alias": "claude-opus",
      "mapper": "bedrock",
      "scheme": "https",
      "host": "bedrock-runtime.us-east-1.amazonaws.com",
      "path": "/model",
      "model": "global.anthropic.claude-opus-4-8",
      "aws_region": "us-east-1",
      "drop_request_fields": ["temperature", "top_p"],
      "log_crop_request_fields": ["messages.content.text"],
      "log_crop_limit": 200
    },
    {
      "name": "openai-mini",
      "alias": "gpt-mini",
      "mapper": "openai",
      "scheme": "https",
      "host": "api.openai.com",
      "path": "/",
      "model": "gpt-5.4-mini-2026-03-17",
      "headers": [
        { "key": "Authorization", "value": "Bearer ${OPENAI_API_KEY}" }
      ]
    }
  ]
}
```

| Field                                                         | Meaning                                                                   |
| ------------------------------------------------------------- | ------------------------------------------------------------------------- |
| `name`                                                        | Stable `backend_id` for logs, quota and queues. Defaults to `model@host`. |
| `alias`                                                       | Routing label matched against `X-Request-Model`.                          |
| `mapper`                                                      | `openai`, `anthropic`, `bedrock`, `grok` or `passthrough` (default).      |
| `scheme` / `host` / `path`                                    | Upstream URL.                                                             |
| `model`                                                       | Model injected into the request (or into the path, for Bedrock).          |
| `max_tokens`                                                  | Default `max_tokens` when the client omits it (`anthropic`, `bedrock`).   |
| `aws_region`, `aws_service`, `aws_api_key`                    | Bedrock auth: SigV4 from the AWS chain, or a bearer API key.              |
| `headers`, `queries`                                          | Extra headers and query params sent upstream.                             |
| `drop_request_fields`                                         | Fields stripped from the body before forwarding.                          |
| `client_tag_header`                                           | Header whose value tags the caller in logs (`untagged` if absent).        |
| `log_{drop,crop}_{request,response}_fields`, `log_crop_limit` | Reshape bodies **in logs only** — the upstream body is untouched.         |

> Keep API keys out of version control — pass the config through the environment.

### Environment

| Variable                                                       | Default      | Purpose                                                             |
| -------------------------------------------------------------- | ------------ | ------------------------------------------------------------------- |
| `PROXY_CONFIG`                                                 | —            | Inline JSON config.                                                  |
| `PORT`                                                         | —            | Proxy listen port (`3000` in the Docker images).                    |
| `ADMIN_PORT`                                                   | `9090`       | Internal admin server: `GET /config`, `/debug/pprof/*`.             |
| `APPCONFIG_APP_ID`, `APPCONFIG_ENV_ID`, `APPCONFIG_PROFILE_ID` | —            | Load config from the AWS AppConfig Agent instead of `PROXY_CONFIG`. |
| `APPCONFIG_POLL_INTERVAL`, `APPCONFIG_BOOT_TIMEOUT`            | `30s`, `10s` | AppConfig polling and startup wait.                                 |
| `HEARTBEAT_INTERVAL`                                           | `30s`        | Periodic queue heartbeat log.                                       |
| `ENV`                                                          | —            | `local` for pretty logs; anything else emits JSON.                  |
| `LOG_LEVEL`, `SERVICE_NAME`                                    | —            | Logger tuning.                                                      |

## Endpoints

| Path                 | What it serves                                                     |
| -------------------- | ------------------------------------------------------------------ |
| `/*`                 | Proxied to the selected backend.                                   |
| `/health`            | Liveness check.                                                    |
| `/dashboard`         | Live request dashboard (SSE from `/request-events`).               |
| `/quotes`, `/status` | Quota pages (SSE from `/events`).                                  |
| `:9090/config`       | Current backend config, header/query values redacted (admin port). |
| `:9090/debug/pprof/` | Go profiler (admin port).                                          |

## Development

```bash
go tool gotestsum ./internal/...   # unit tests
air                                # run with hot reload (.air.toml)
mockery                            # regenerate mocks (.mockery.yml) — never edit internal/mocks by hand
```

Single test: `go test ./internal/proxy/ -run TestName`.

## Docker

```bash
docker build -t spillway .
docker run -p 3000:3000 -e PORT=3000 -e PROXY_CONFIG="$(jq -c . config.json)" spillway
```
