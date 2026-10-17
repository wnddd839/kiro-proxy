# Configuration

The config file is optional. Missing fields use the defaults below.

Default path: `kiro-proxy.json` in the working directory. A legacy
`kiro-go.json` is used only when `-config` is not passed and the new file does
not exist.

```json
{
  "listen": "127.0.0.1:8787",
  "accounts_file": "accounts.json",
  "keys_file": "keys.json",
  "usage_file": "usage.jsonl",
  "cache_mode": "auto",
  "session_ttl": "24h"
}
```

## Startup safety

- A non-loopback listener without any downstream key is refused. Use
  `-allow-insecure-listen` only when you understand the exposure.
- A blank key in `api_keys` is rejected. An empty key would compare equal to a
  missing credential.
- `/admin` only accepts loopback requests unless `admin_token` is set. Forwarding
  headers disqualify a request from being treated as local.
- Debug request snapshots are off by default. When enabled they are redacted and
  size/age bounded, but still contain conversation text.

## Fields

### Server and files

| Field | Default | Description |
|---|---|---|
| `listen` | `127.0.0.1:8787` | Listen address. Non-loopback without keys is refused. |
| `accounts_file` | `accounts.json` | Account pool. Contains tokens. Created `0600`. |
| `api_keys` | empty | Full-access keys without budgets. Intended for self-use. |
| `keys_file` | `keys.json` | Managed keys with budgets. Stores only SHA-256. |
| `usage_file` | `usage.jsonl` | Usage journal. Empty string keeps it in memory only. |
| `usage_retention` | `2160h` | Detail and query retention (90 days). Lifetime budgets survive. |
| `admin_token` | empty | `X-Admin-Token` for `/admin`. Empty means loopback-only. |

### Cache and metering

| Field | Default | Description |
|---|---|---|
| `cache_mode` | `auto` | `auto` simulates a 5m prefix cache for all protocols. `protocol` uses client breakpoints for Messages and auto for OpenAI. `explicit` only honors client breakpoints. `off` disables simulation. |
| `cache_ttl` | `client` | In explicit/protocol modes, honor a client `ttl: "1h"` (`client`) or always bill 5m (`5m`). `auto` always uses 5m. |
| `cache_points` | empty | Experimental: send explicit `cachePoint{type:"default"}` at `first-user`, `assistant`, or `tools`. Probes showed no effect; leave off. |
| `cache_file` | `promptcache.json` | Prefix fingerprints and session state. Empty keeps state in memory. |
| `sort_tools` | `true` | Sort tool declarations by name. MCP load order is not stable. |
| `system_strip` | empty | Regexes removed from system. Confirm they break the cache before adding. |
| `pin_thinking` | `true` | Pin legacy thinking budgets per thread. Native effort/thinking is pinned per thread and model. |
| `conversation_mode` | `session` | `random` rotates conversationId each request for cache A/B tests. |
| `session_ttl` | `24h` | Idle lifetime for session → account and conversationId pins. |
| `reported_usage` | `conservative` | How upstream token reports are applied: `conservative`/`raw` take the last value per field, `sum` accumulates, `ignore` uses local metering only. |
| `hidden_tokens_billing` | `input` | `input` bills Kiro's hidden context at the input-side cache state. `off` does not bill it but still records it. |
| `hidden_tokens` | empty | Exact model id → hidden token count. Built-in measured values are used for known models; others fall back to Claude's 4052 and are marked uncalibrated. |
| `unknown_model_pricing` | `highest` | `highest` warns and bills unknown models at the highest known component price. `reject` refuses them before generation. |

### Prices and cost

| Field | Default | Description |
|---|---|---|
| `credit_rates` | built-in fitted values | Model prefix → per-million-token credit coefficients. Used when upstream reports no credits, and for budget reservation. |
| `cost_basis` | `api` | `api` bills downstream at official Claude API prices. `credits` uses credits × `credit_usd`. |
| `credit_usd` | `0.02` | USD value of one Kiro credit. Used for upstream cost and margin. |
| `prices` | built-in | Override table: model id prefix → `{"input","output","cache_write","cache_write_1h","cache_read"}` in USD per million tokens. |
| `price_sync` | `24h` | Online price sync interval (OpenRouter only, filtered to Kiro models). `0` uses the built-in table only. |
| `prices_file` | `prices.json` | Online price cache. Empty keeps it in memory. |

### Upstream identity and compatibility

| Field | Default | Description |
|---|---|---|
| `identity` | kiro-cli 2.28.0 | CLI version, API version, and desktop UA reported upstream. Aligned with kiro-cli for compatibility. |
| `model_aliases` | empty | Downstream model name → Kiro model id. |
| `openai_hosted_tools` | `drop` | OpenAI built-in tools such as `web_search`: `drop` skips them and logs at debug, `reject` returns 400. Codex sends `web_search` by default. |

### Pool and timeouts

| Field | Default | Description |
|---|---|---|
| `max_attempts` | `3` | Maximum accounts tried for one request. |
| `max_concurrent` | `3` | Per-account concurrency unless the account overrides it. `0` means unlimited. |
| `limits_interval` | `0s` | Periodic quota polling. Off by default; a quota error polls that account only. |
| `breaker_window` | `2m` | Sliding window. A failure class above 50% across at least two accounts trips the breaker. It cools accounts but does not disable them. |
| `upstream_connect_timeout` | `15s` | TCP and TLS setup timeout. |
| `upstream_first_byte_timeout` | `3m` | Time to response headers and first body byte. Cold long-context requests can exceed a minute. |
| `upstream_idle_timeout` | `120s` | Maximum gap between upstream bytes. Client pings do not extend it. |
| `server_read_timeout` | `30s` | Downstream request read timeout. Header timeout is `min(10s, read timeout)`. Idle timeout is 60s. |
| `log_level` | `info` | `debug`, `info`, `warn`, or `error`. |

### Debug snapshots

| Field | Default | Description |
|---|---|---|
| `debug_requests_dir` | empty | Save redacted upstream requests and prefix comparisons. Off by default. |
| `debug_requests_max_bytes` | `67108864` | Total size cap (64 MiB). |
| `debug_requests_retention` | `24h` | Age cap. Cleaned at startup, on write, and every 10 minutes. |

## Environment variables

| Variable | Effect |
|---|---|
| `KIRO_ADMIN_TOKEN` | Overrides `admin_token`. |
| `KIRO_API_KEY` | Appends one full-access key to `api_keys`. |

These are read at startup. They are not written back to the config file.
