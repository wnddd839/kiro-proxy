# Operations and troubleshooting

## What the console shows

| Page | Contents |
|---|---|
| 概览 | Hit rate, pool counts, today/month credits, TPS, prompt and hit-rate trend, effective config. |
| 号池 | Add accounts, import IDE credentials, refresh, enable/disable, test, pull quota, cooldowns. |
| 用量账单 | Filters by date/key/account/protocol/model; credits, hit rate, TPS; daily chart; CSV export. |
| 接入 | Base URLs, recommended model, generate/rotate keys, environment and curl examples. |
| 请求 | The last 500 upstream attempts: account, thread, conversation, cache read/write, credits, TPS, error. |
| 模型 | Credit multiplier, context window, cache support, price source, hidden-token calibration. |

The page itself contains no data. It calls `/admin/*`. If `admin_token` is set,
the page asks for it once and stores it in `localStorage`.

## Account lifecycle

### Sign-in

- Browser OAuth via `login` or the console.
- Import the Kiro IDE token file with `import-ide`.
- Paste an access token or API key manually.

Do not add the same Kiro account through both `login` and `import-ide`. Each
path refreshes independently and can invalidate the other's refresh token. The
symptom is an account disabled with `sign-in expired`.

On a server, use the manual callback flow described in
[Deployment](deployment.md#sign-in-inside-a-container).

### Refresh

- One refresh at a time per account.
- `401` or an explicit token-invalid `403`: refresh, then retry the same account.
- `invalid_grant` from refresh: disable the account and keep the record. Sign in
  again to replace it.
- Other refresh `4xx`: cooldown and count. Three consecutive failures disable it.
- Temporary network failures only cool the account. They do not disable it.

### Quota

- A quota error cools the account until the reset time, or one hour when the
  reset time is unknown.
- `limits_interval` is off by default. A quota error triggers a single
  `Get-Usage-Limits` call for that account.
- The console's "拉额度" button refreshes one account; batch actions do all.

### Failure classes

| Upstream | Action |
|---|---|
| `401`, token-invalid `403` | Single-flight refresh, then retry the same account |
| Other `403` | Do not refresh; short cooldown, switch account |
| Refresh `invalid_grant` | Disable, keep record, ask for sign-in |
| Suspension (`TemporarilySuspended`, `423`) | Disable, keep record |
| Quota (`402` or explicit quota text) | Cool until reset, switch account |
| `429` throttling | Cool by `x-amzn-kiro-ratelimit-retry-after` or exponential backoff |
| Suspicious-activity `429`/`403` | 24-hour soft ban, alert |
| `5xx`, network | Short cooldown, switch account |
| Input too long, validation | Return directly; do not switch |
| All unavailable | `429` with `Retry-After` |

A global breaker opens when the same failure class exceeds 50% of request
results in the window and involves at least two accounts. It cools accounts but
does not disable them. Definite suspension or invalid credentials still disable
the account.

## Common problems

### Claude Code connects but every request fails

Check the base URL. Anthropic clients need the origin, not `/v1`:

```sh
export ANTHROPIC_BASE_URL=http://127.0.0.1:8787
```

OpenAI clients need `/v1`:

```sh
export OPENAI_BASE_URL=http://127.0.0.1:8787/v1
```

If keys are configured, send the same key in `x-api-key` or
`Authorization: Bearer`.

### `admin is only reachable from this machine`

The console is bound to loopback. Either open it on the host, or set
`admin_token` / `KIRO_ADMIN_TOKEN` and use the token field. Behind a reverse
proxy, the forwarding headers mean the request is never treated as local.

### Login hangs on a server

The OAuth callback points at `localhost` on the browser machine, not the server.
Use the paste-callback flow in the console or CLI.

### An account is disabled with `sign-in expired`

The refresh token is no longer valid. This can happen when:

- The account was added through both `login` and `import-ide`.
- The IDE or another client refreshed the same token.
- The account was signed out or its sessions revoked.

Sign in again, then delete the old record.

### Cache hit rate is low

Check in this order:

1. Is `conversation_mode` set to `session`?
2. Are requests falling on the same account? The request page shows the account
   and a pinned flag.
3. Does the conversationId stay the same? The request page shows a short hash.
4. Does the tool set or system prefix change between turns? Enable
   `debug_requests_dir` and inspect the first differing block.
5. Is the gap between turns longer than about 5 minutes?
6. Is the request below the minimum cacheable length?

Short sessions always look bad: the first turn writes the cache and cannot read
it. Look at the multi-turn curve, not a single request.

### `temperature` warnings

Kiro does not forward `temperature`. The proxy logs once and continues. Do not
expect deterministic sampling.

### `web_search` is missing

OpenAI hosted tools cannot be forwarded. The default drops them. Set
`openai_hosted_tools: "reject"` if you prefer an explicit error.

`tool_choice` and `top_p` are also not forwarded.

### Output stops early

If a client sends `max_tokens`, the proxy enforces it locally over text,
thinking, and tool wrappers. Raise the limit or omit it. Discarded output is not
billed, but upstream work already happened.

### Journal or health failures

`/health` returns `503` when the usage journal cannot write or the pending
backlog reaches 32 MiB. Budgeted keys pause new generation; anonymous and
`api_keys` traffic continues. Check disk space and file permissions. The journal
retries in the background and resumes service after a successful write.

### The console shows different credits than the upstream

Credits are usually reported by upstream `meteringEvent`. When the stream ends
before that event, the proxy estimates from tokens and marks the row
`credits_estimated`. The reconciliation table compares `Get-Usage-Limits`
deltas with recorded credits. A positive unassigned value means upstream
charged credits the proxy did not record, for example from interrupted requests
or other clients sharing the account.

## Logs

Logs go to stderr. They are passed through a sanitizing handler that removes
credential-like values and secret fields. Natural-language secrets cannot be
detected reliably, so do not put prompts or tokens in log messages.

Increase detail with:

```json
{"log_level": "debug"}
```

Debug request snapshots are separate and off by default. When enabled, files
still contain redacted conversation text.

## Backup

See [Deployment: Upgrade and backup](deployment.md#upgrade-and-backup).

## Reporting an issue

Include:

- Platform and binary/image version.
- Client and version (Claude Code, Codex, Cursor, SDK, ...).
- The protocol you used.
- Whether keys are configured.
- Relevant logs with credentials removed.
- What the console's request row shows: status, account, pinned, cache read /
  write, error.

Do not paste account tokens, refresh tokens, or full request bodies.
