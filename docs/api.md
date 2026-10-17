# API

Base URL: `http://127.0.0.1:8787`.

The proxy accepts both Anthropic and OpenAI wire formats on the same port.
Path prefixes are tolerant: `/messages`, `/v1/messages`, `/v1/v1/messages`,
`/anthropic/v1/messages`, trailing slashes, and repeated slashes all reach the
same handler.

Authentication uses either header:

```http
x-api-key: sk-kiro-...
Authorization: Bearer sk-kiro-...
```

When no key is configured and the listener is loopback, requests are accepted
anonymously. Once any key has existed, deleting the last key does not reopen
anonymous access.

## Anthropic Messages

### `POST /v1/messages`

Streaming and non-streaming. Tool use and extended thinking content blocks are
translated.

```sh
curl http://127.0.0.1:8787/v1/messages \
  -H "content-type: application/json" \
  -H "x-api-key: $KIRO_API_KEY" \
  -H "anthropic-version: 2023-06-01" \
  -d '{
    "model": "claude-sonnet-4.5",
    "max_tokens": 256,
    "messages": [{"role": "user", "content": "ping"}]
  }'
```

### `POST /v1/messages/count_tokens`

Returns a local estimate shaped like the Anthropic response. Kiro has no
count-tokens endpoint, so this uses the same tokenizer and cache rules as
billing.

```json
{"input_tokens": 12, "hidden_tokens": 4052}
```

### `GET /v1/models`

Lists models from the pool's upstream catalog. If no account can list models,
the endpoint returns `503` rather than inventing a list.

## OpenAI Chat Completions

### `POST /v1/chat/completions`

```sh
curl http://127.0.0.1:8787/v1/chat/completions \
  -H "content-type: application/json" \
  -H "authorization: Bearer $KIRO_API_KEY" \
  -d '{
    "model": "claude-sonnet-4.5",
    "messages": [{"role": "user", "content": "ping"}]
  }'
```

`usage` includes standard fields plus extensions:

- `cached_tokens` in `prompt_tokens_details`
- `cache_read_input_tokens`
- `cache_creation_input_tokens`
- `credits`

`stream_options.include_usage` is honored. When omitted, a final usage chunk is
sent.

## OpenAI Responses

### `POST /v1/responses`

```sh
curl http://127.0.0.1:8787/v1/responses \
  -H "content-type: application/json" \
  -H "authorization: Bearer $KIRO_API_KEY" \
  -d '{
    "model": "claude-sonnet-4.5",
    "input": "ping"
  }'
```

`previous_response_id` is not supported. Send the full input each turn.

OpenAI hosted tools such as `web_search` cannot be forwarded to Kiro. The
default `openai_hosted_tools: "drop"` skips them and logs at debug; set
`"reject"` to return a 400 instead.

## Behavior notes

### Generation limits

Kiro does not accept `max_tokens`, `max_completion_tokens`, or
`max_output_tokens`. The proxy enforces the limit locally over delivered text,
thinking, and tool wrappers. Reaching the limit cancels the upstream request and
returns `max_tokens` (`length` for Chat, incomplete for Responses).

If no limit is provided, there is no local cap. Budget reservation still uses a
4096-token output estimate.

### Stop sequences

Anthropic `stop_sequences` and Chat `stop` are matched across stream events. The
matched sequence and everything after it are removed. The response reports
`stop_sequence`.

### Ignored parameters

- `temperature` is not forwarded. The first occurrence logs a warning, later
  occurrences log at debug.
- `top_p` is not supported.
- `tool_choice` is parsed but not forwarded.

### Streaming usage

For Anthropic streaming:

- `message_start.usage` is zero because final input is not known yet.
- `message_delta.usage` carries the settlement.
- `message_delta.settled_usage` repeats the same value for older integrations.

Do not add `message_start.usage` and `message_delta.usage`.

## Admin API

All admin endpoints require loopback access or `X-Admin-Token`.

| Method | Path | Purpose |
|---|---|---|
| `GET` | `/health` | Liveness and journal health. `503` when writes are failing. |
| `GET` | `/admin/overview` | Totals, pool counts, config, breaker state. |
| `GET` | `/admin/stats` | Pool cumulative totals and hit rate. |
| `GET` | `/admin/accounts` | Pool list without tokens. |
| `POST` | `/admin/accounts` | Add an account or import the IDE token. |
| `DELETE` | `/admin/accounts/{id}` | Remove an account. |
| `POST` | `/admin/accounts/{id}/{enable\|disable\|refresh}` | Per-account actions. |
| `POST` | `/admin/accounts/batch` | `{"action":"test\|limits\|refresh\|enable\|disable\|delete","ids":[]}`. Empty ids means all; delete requires ids. |
| `GET` | `/admin/requests?limit=N` | Recent upstream attempts. |
| `GET` | `/admin/keys` | Keys with period spend. |
| `POST` | `/admin/keys` | Create a key. |
| `PATCH` | `/admin/keys/{id}` | Update a key. |
| `DELETE` | `/admin/keys/{id}` | Delete a key. |
| `POST` | `/admin/keys/{id}/rotate` | Replace the secret. |
| `GET` | `/admin/usage` | Aggregated usage. |
| `GET` | `/admin/billing` | Billing summary. Defaults to the current month. |
| `GET` | `/admin/prices` | Effective price table. |
| `POST` | `/admin/prices/refresh` | Refresh online prices. |
| `GET` | `/admin/models` | Model catalog. |
| `POST` | `/admin/models/refresh` | Refresh the catalog. |
| `GET/POST/DELETE` | `/admin/login` | Sign-in status, start, cancel. |
| `POST` | `/admin/login?callback=<url>` | Submit a callback URL manually. |

### Add an account

```json
{"label":"b","cred":{"method":"social","access_token":"...","refresh_token":"...","region":"us-east-1"}}
{"label":"c","cred":{"method":"idc","refresh_token":"...","client_id":"...","client_secret":"...","region":"us-east-1"}}
{"label":"k","cred":{"access_token":"ksk_..."}}
{"import":"ide","path":"/path/to/kiro-auth-token.json"}
```

### Create a key

```json
{
  "name": "team-a",
  "limit_usd": 20,
  "limit_credits": 500,
  "limit_requests": 1000,
  "period": "month",
  "rpm": 30,
  "models": ["claude-*"],
  "expires_at": "2026-12-31T00:00:00Z",
  "note": "internal team"
}
```

`period` is `day`, `week`, `month`, or `total`. Budget errors return `402` with
`x-should-retry: false`.

### Query parameters

`/admin/usage` and `/admin/billing` accept:

- `from`, `to`: RFC3339, `YYYY-MM-DD`, or a span such as `24h` / `7d`
- `key`, `account`, `model`, `protocol`, `status`
- `bucket`: `hour` or `day`
- `page`, `page_size` (usage details only)

## Error shapes

Anthropic endpoints return:

```json
{"type":"error","error":{"type":"rate_limit_error","message":"..."}}
```

OpenAI endpoints return:

```json
{"error":{"message":"...","type":"rate_limit_error","code":"rate_limit_exceeded"}}
```

Rate-limit responses include `Retry-After`. Budget exhaustion returns `402`.
