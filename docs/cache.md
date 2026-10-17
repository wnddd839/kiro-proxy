# Cache routing

This is the reason the project exists. Everything else supports it.

## The problem

Kiro runs its own prompt cache upstream. Two things break it:

1. **The request shape changes.** A different `conversationId`, reordered tool
   declarations, a changing system header, or a thinking budget that moves
   bytes at the front of the prompt makes the upstream see a new prefix.
2. **The conversation moves between accounts.** Upstream cache belongs to the
   account that served the earlier turns. Round-robin routing throws it away.

Most Kiro proxies forward requests correctly and still miss the cache. The cost
difference is large: on a warm thread, most of the input is billed as a cache
read instead of full input.

## What this proxy does

```text
client request
     │
     ├─ session key: X-Session-Id / prompt_cache_key / system + first user hash
     │
     ├─ account pin: session → account       (no round-robin)
     │
     ├─ conversation pin: session + account → conversationId
     │
     ├─ prefix stabilization:
     │     strip x-anthropic-billing-header
     │     sort tool declarations by name
     │     canonicalize JSON object key order
     │     pin native effort / thinking per thread and model
     │
     └─ send full history upstream, on the same account and conversationId
```

The upstream still receives the full history. The proxy does not store model
answers locally. It makes the request shape stable enough for Kiro's own cache
to hit.

### Session key

In order of preference:

1. Client headers: `X-Session-Id`, `X-Opencode-Session`,
   `X-Conversation-Id`, `Conversation-Id`, `Session-Id`.
2. `metadata.user_id` / `prompt_cache_key`.
3. A stable hash of the system prefix plus the first user message.

Later tool turns do not change the key. If two clients happen to share the same
first user message and system, they share a key. That is rare and less harmful
than round-robin routing.

### Account pin

The session stays on the account that served the first successful turn. If that
account is busy, the request waits up to 3 seconds and then returns `429` with
`Retry-After`. It does not borrow another account, because a different account
cannot see the previous account's cache.

The pin is released only when an account becomes unavailable: cooldown,
disabled, quota exhausted, or a failed attempt. When the pin moves, the
conversationId is rotated for the new account.

### Stabilization

- `x-anthropic-billing-header` is stripped from system. Claude Code changes
  `cch` on every request.
- Tool declarations are sorted by name and JSON object keys are canonicalized.
  Arrays and numeric precision are preserved.
- Historical thinking blocks that are not forwarded do not participate in the
  session fingerprint.
- Historical images are kept. New images do not remove old bytes.
- Native effort and thinking fields are pinned to the first request in a
  thread/model pair. Switching strength mid-thread was measured to cool Sonnet
  4.6; later tests on Sonnet 5.5 stayed warm. The proxy pins conservatively
  rather than assuming cross-strength reuse.
- `system` append keeps the thread key. A replacement rotates it and records
  `thread_key_change_reason`.

### TTL

Measured in October 2026 on `claude-sonnet-4.5`: the upstream cache lives about
5 minutes. Requests 0 and 4 minutes later hit; 6 minutes later did not. A client
`ttl: "1h"` does not change the upstream TTL in these tests. `cache_mode: auto`
therefore budgets 5m.

`cache_mode: explicit` or `protocol` follows client breakpoints and 1h
declarations for local billing. That is a billing model, not a claim that the
upstream honors the longer TTL.

## Measuring it

The console shows cache read, cache write, input, and hit rate per account and
per request. For a controlled comparison, switch `conversation_mode` between
`session` and `random` and compare the same multi-turn workload:

```json
{"conversation_mode": "random"}
```

`random` generates a new conversationId per request. The cache-read share should
collapse. If it does not, the upstream is not using conversationId or prefix
matching the way the model suggests.

The `probe` command measures upstream behavior directly:

```sh
kiro-proxy -config kiro-proxy.json probe usage
kiro-proxy -config kiro-proxy.json probe ttl
kiro-proxy -config kiro-proxy.json probe cachepoint
kiro-proxy -config kiro-proxy.json probe credits
kiro-proxy -config kiro-proxy.json probe analyze -out probe.jsonl
```

These calls spend credits against the selected account.

## What breaks a hit

- The client rewrites early messages (compaction, history edit, revert).
- System content changes before the first stable point.
- The tool set changes, even by adding one tool.
- A client sends a different thinking budget every turn.
- The session moves to another account.
- More than about 5 minutes pass between requests.
- A request is so short it falls below the minimum cacheable length.

The proxy reports the first changed block in debug snapshots so you can see
which of these happened.

## What it deliberately does not do

- No local answer cache. Skipping upstream would have to reproduce tool calls,
  refusals, and streaming; and cached answers can leak across accounts.
- No cross-account cache sharing. Upstream cache is per account.
- No prompt rewriting that hides real changes. If your client edits history,
  the request reflects it.
- No claim that byte stability guarantees a hit. Account, model parameters,
  upstream TTL, and server-side behavior also matter.

## Further reading

- [Cache strategy notes](notes/cache-strategy.md)
- [Prefix stability measurements](notes/prefix-stability.md)
- [Native thinking measurements](notes/native-thinking.md)
