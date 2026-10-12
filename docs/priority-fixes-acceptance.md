# Kiro-proxy priority fixes acceptance (2026-10-10)

> Historical acceptance snapshot. The subsequent review changed authentication,
> lifetime budgets, reservations, stream usage, journal fault handling and the
> last-account policy. See [review-fixes.md](review-fixes.md) and README.md for
> current behavior; the status table below describes the earlier snapshot.

## Scope and verification

No git staging or commits. No production startup, live account probing, or edits
to `bin/accounts.json`, `data/`, or actual credential/key files. All HTTP upstreams
and account fixtures in regression tests are synthetic and use temporary paths.

Groups 1 and 2 passed their formatting, vet and full race-test gates before the
next group. Group 3 includes cross-review corrections to earlier groups. Final
snapshot verification is repeated after the last correction, rather than relying
on the subagents' reports. Commands:

```powershell
gofmt -w <changed and new Go files>
gofmt -l <changed and new Go files>
go vet ./...
go test -race ./... -count=1 -timeout 180s
go build ./...
go test -race ./internal/pool ./internal/server -run 'Concurrent|Reservation|Breaker|UpstreamTimeoutsReleaseResources|StateSidecar' -count=3 -timeout 180s
```

The default Go cache was inaccessible under the sandbox. Verification uses
workspace-local `GOCACHE` / `GOTMPDIR`; this does not change application config.

## Issue status and behavior changes

| Issue | Status | Implementation and remaining limitations |
|---|---|---|
| 1: hidden tokens/calibration | Fixed | Separate wire/journal `hidden_tokens` and `hidden_tokens_charged_usd`; `input`/`off` billing; exact-model overrides, default 4052. Output never scaled; floor client input categories independently without residual input. Hidden excluded from hit denominator. Reported input, including pre-content failures, separates hidden under a documented whole-context assumption. |
| 2: message_start usage | Partially mitigated | Nonzero local input/cache estimate, output 0, consistent monotonic final display; `message_delta.settled_usage` contains actual final accounting. Initial local estimate cannot be a provable lower bound of an arbitrary smaller upstream report; standard final display can exceed actual settlement. Billing clients must read the extension. |
| 3: online/unknown prices | Fixed | Missing online cache fields use builtin applicable context-tier prices, not input price. Warnings in logs/model table. Unknown models use component-wise highest known rates by default, or fail before generation with `reject`. |
| 4: budgets | Partially mitigated | Locked admission includes spent + in-flight holds + current request. Reserve full max output, conservative client/cache/context tiers, hidden and historical spend. Actual journal settlement and release serialized. Arbitrarily large upstream reported input/credits can still exceed the estimate; not a hard USD/credits ceiling. |
| 5: local generation controls | Fixed | Local aggregate text/thinking/tool output cap, UTF-8 safe; stop sequences matched across text events and excluded from delivery/billing. Default max output 4096. Temperature logged as ignored, not forwarded. Upstream may already have done discarded work. |
| 6: upstream timeouts | Fixed | Connect/TLS 15s, first-byte 60s, idle 120s by default; configurable. Header-only response retains first-byte deadline. Idle resets on upstream bytes, not downstream pings. Cancel/close precedes reader join; leases/holds released. Retried failures are free; delivered partial streams use interruption billing. |
| 7: prefix stability | Fixed | Native effort pinned to first thread/model settings. Appended system retains original key; replacement records thread-key reason. Stored prefixes contain only digest/length/opaque key. Redacted debug snapshots and first-different-block index/role/type/80-character preview. |
| 8: pool resilience | Fixed | Rolling request-result failure ratio >50% and at least two enabled failing accounts. Success adds denominator without clearing history. Last enabled account never auto-disabled or globally tripped; may still be cooled and temporarily unavailable. Pre-trigger refresh strikes rolled back. Atomic nonsecret state sidecar restores valid cooldown/stats/window/strike timing. |
| 9: risk-control soft bans | Fixed | Suspicious activity HTTP 429/403 and stream errors receive distinct 24h cooldown and alert, not ordinary <=5m exponential throttle. |
| 10: security | Fixed | Unauthenticated nonloopback listener refused unless explicitly overridden with warning. Blank configured keys rejected. HTTP read/header/idle deadlines. Debug off by default, managed snapshot cap64MiB/retention24h, startup/write/10min cleanup. Logs sanitize structured bodies, credential-like strings, secret attributes and errors; debug images/profile/credentials redacted. Natural-language secrets are not perfectly detectable. |

No issue is classified as wholly unfixed. Issues 2 and 4 are not claimed to fully
satisfy the requested mathematical lower-bound/hard-budget guarantees.

## Eight-round synthetic comparison

Source: `D:/kiro-go/internal/server/priority_test.go`,
`TestEightRoundUsageScenarios`. Real growing local message history and local
token/cache functions; fake upstream context reports. No live model measurement.
"Before input" reconstructs the old formula (client uncached input +4052), not
execution of a checked-out old binary.

### Warm thread, default auto cache

| Round | Client prompt | Before input | After input | Cache read | Cache write | Output | Ledger hidden | Hidden USD |
|---|---:|---:|---:|---:|---:|---:|---:|---:|
| 1 | 1700 | 4052 | 0 | 0 | 1700 | 12 | 4052 | 0.012156 |
| 2 | 3242 | 4052 | 0 | 1700 | 1542 | 23 | 4052 | 0.012156 |
| 3 | 4786 | 4052 | 0 | 3242 | 1544 | 34 | 4052 | 0.012156 |
| 4 | 6328 | 4052 | 0 | 4786 | 1542 | 45 | 4052 | 0.012156 |
| 5 | 7872 | 4052 | 0 | 6328 | 1544 | 56 | 4052 | 0.012156 |
| 6 | 9414 | 4052 | 0 | 7872 | 1542 | 67 | 4052 | 0.012156 |
| 7 | 10958 | 4052 | 0 | 9414 | 1544 | 78 | 4052 | 0.012156 |
| 8 | 12500 | 4052 | 0 | 10958 | 1542 | 89 | 4052 | 0.012156 |

Auto's last-block breakpoint legitimately places all client input in cache
categories; uncached input 0 is correct, not a replacement hidden baseline. Read
tracks previous context, write tracks new suffix, and output tracks generated
text. Prompt sum excludes hidden, and each row stores hidden independently.

### Cold start every round, and uncached miss (cache off)

| Round | Cold input/read/write | Miss before input | Miss after input/read/write | Output | Ledger hidden |
|---|---|---:|---|---:|---:|
| 1 | 0 / 0 / 1700 | 5752 | 1700 / 0 / 0 | 12 | 4052 |
| 2 | 0 / 0 / 3242 | 7294 | 3242 / 0 / 0 | 23 | 4052 |
| 3 | 0 / 0 / 4786 | 8838 | 4786 / 0 / 0 | 34 | 4052 |
| 4 | 0 / 0 / 6328 | 10380 | 6328 / 0 / 0 | 45 | 4052 |
| 5 | 0 / 0 / 7872 | 11924 | 7872 / 0 / 0 | 56 | 4052 |
| 6 | 0 / 0 / 9414 | 13466 | 9414 / 0 / 0 | 67 | 4052 |
| 7 | 0 / 0 / 10958 | 15010 | 10958 / 0 / 0 | 78 | 4052 |
| 8 | 0 / 0 / 12500 | 16552 | 12500 / 0 / 0 | 89 | 4052 |

Every cold/miss ledger row also has hidden USD 0.012156 at Sonnet $3/M input.
`hidden_tokens_billing=off` keeps the hidden count but sets hidden USD to 0.
Opus 5.5 remains 4052 and uncalibrated. Obtain a real Opus account and run the
probe with `-window 1000000` before changing its baseline.

## New regression coverage

Group 1:
- `TestCalibrationDoesNotInventInputOrScaleOutput`, missing regular/long cache prices.
- `TestEightRoundUsageScenarios` hit/cold/miss; separate hidden wire/journal,
  override/off, unknown highest/reject, reported whole input/tiny totals,
  pre-content terminal input, streaming actual settlement.
- Concurrent USD/current-request admission, full output and expensive cache/long
  context reservations; failed attempts remain free on retry.

Group 2:
- `TestLocalOutputLimitsHTTP`, cross-event stop matching and pending stop prefix
  before tools; text/thinking/tool accounting.
- `TestUpstreamTimeoutsReleaseResources` headers/firstbyte/idle; partial charging,
  lease and hold release; client cancellation no longer strikes an account.
- System append/replacement, native pinning, restart continuity and system-text
  privacy, redacted prefix comparison.

Group 3:
- Rolling failure ratio, single success does not clear it, last enabled account
  even with old disabled-account history, real pre-trigger strike rollback.
- Cooldown/stat/window restart, expiry, concurrent atomic writes; last-account
  banned/invalid-grant refresh cooldown survives restart.
- Suspicious HTTP/stream classification and 24h cooldown.
- Wildcard security guard, read/header/idle deadlines, read-only usable-key
  inspection, blank key rejection, safe logger/with/group/error boundaries.
- Debug cap/oversize/keep-existing, zero-byte cleanup, prune-failure skip,
  retention/pairs/orphans/concurrent cleanup, image/token redaction.

## Compatibility and remaining operational limits

- Stream start and delta are cumulative, not additive. Standard-only consumers
  can display an overestimate; token-billing integrations need `settled_usage`.
- Reported input currently assumes a whole-context total that includes hidden;
  a future probe must confirm each upstream/model's actual convention.
- Budget estimates do not bound arbitrarily large upstream metering events.
  A hard guarantee requires a separately agreed billing cap or upstream bound.
- Pool reconciliation poll baselines and lastUsed are not persisted; primary
  cooldown/statistics and breaker state are. No credentials in the sidecar.
- Debug contains redacted conversation/tool material and must still be treated
  as sensitive. Only managed proxy snapshots count toward cleanup; unrelated
  files are preserved. Atomic writes use temporary copies; crashes may leave
  temporary files, which are not a license to delete arbitrary shared files.

## Changed file inventory

- `D:/kiro-go/cmd/kiro-proxy/main.go`
- `D:/kiro-go/cmd/kiro-proxy/security_test.go`
- `D:/kiro-go/docs/priority-fixes-acceptance.md`
- `D:/kiro-go/docs/superpowers/plans/2026-10-10-priority-fixes.md`
- `D:/kiro-go/internal/anthropic/types.go`
- `D:/kiro-go/internal/config/config.go`
- `D:/kiro-go/internal/config/security_test.go`
- `D:/kiro-go/internal/kiro/auth.go`
- `D:/kiro-go/internal/kiro/failure_test.go`
- `D:/kiro-go/internal/kiro/failure.go`
- `D:/kiro-go/internal/kiro/prefix.go`
- `D:/kiro-go/internal/kiro/redact_test.go`
- `D:/kiro-go/internal/kiro/redact.go`
- `D:/kiro-go/internal/meter/meter_test.go`
- `D:/kiro-go/internal/meter/online.go`
- `D:/kiro-go/internal/meter/price.go`
- `D:/kiro-go/internal/meter/priority_test.go`
- `D:/kiro-go/internal/meter/tokens.go`
- `D:/kiro-go/internal/meter/ttl_test.go`
- `D:/kiro-go/internal/openai/convert.go`
- `D:/kiro-go/internal/openai/openai.go`
- `D:/kiro-go/internal/pool/pool_test.go`
- `D:/kiro-go/internal/pool/pool.go`
- `D:/kiro-go/internal/pool/priority_test.go`
- `D:/kiro-go/internal/pool/state_regression_test.go`
- `D:/kiro-go/internal/server/admin.html`
- `D:/kiro-go/internal/server/fixes_test.go`
- `D:/kiro-go/internal/server/hidden.go`
- `D:/kiro-go/internal/server/holds.go`
- `D:/kiro-go/internal/server/messages.go`
- `D:/kiro-go/internal/server/metering_test.go`
- `D:/kiro-go/internal/server/native_thinking_test.go`
- `D:/kiro-go/internal/server/prefix_test.go`
- `D:/kiro-go/internal/server/prices.go`
- `D:/kiro-go/internal/server/priority_test.go`
- `D:/kiro-go/internal/server/reply.go`
- `D:/kiro-go/internal/server/server_test.go`
- `D:/kiro-go/internal/server/server.go`
- `D:/kiro-go/internal/server/stability_test.go`
- `D:/kiro-go/internal/server/state_test.go`
- `D:/kiro-go/internal/server/state.go`
- `D:/kiro-go/internal/server/thread.go`
- `D:/kiro-go/internal/server/timeouts.go`
- `D:/kiro-go/internal/server/trace_security_test.go`
- `D:/kiro-go/internal/server/trace.go`
- `D:/kiro-go/internal/server/ttl_test.go`
- `D:/kiro-go/internal/server/usage_events_test.go`
- `D:/kiro-go/internal/turn/turn.go`
- `D:/kiro-go/internal/usage/journal.go`
- `D:/kiro-go/README.md`

## Final verification result

After counted-strike restart persistence was frozen:
- gofmt -l on every changed/new Go file: empty output.
- go vet ./...: exit 0.
- go test -race ./... -count=1 -timeout 180s: exit 0, all test packages passed; pool 5.780s, server 12.291s.
- go build ./...: exit 0.
- Earlier focused concurrent/reservation/breaker/timeout suite repeated three times: both pool and server passed.
- git diff --check: exit 0. Git index empty; protected bin/accounts.json/data absent from diff.

## Verified implementation locations

1. Hidden config: D:/kiro-go/internal/config/config.go:94; calibration: D:/kiro-go/internal/meter/tokens.go:118; ledger fields: D:/kiro-go/internal/usage/journal.go:46; upstream reports: D:/kiro-go/internal/server/reply.go:78; pre-content errors: D:/kiro-go/internal/server/messages.go:543.
2. Initial estimate: D:/kiro-go/internal/server/reply.go:107; monotonic delta and actual settlement: D:/kiro-go/internal/server/reply.go:406.
3. Cache fallback: D:/kiro-go/internal/meter/price.go:217; unknown model and warnings: D:/kiro-go/internal/server/messages.go:154.
4. Locked admission: D:/kiro-go/internal/server/messages.go:226; conservative estimate: D:/kiro-go/internal/server/messages.go:271; worst price category: D:/kiro-go/internal/meter/price.go:295.
5. Local limits and stops: D:/kiro-go/internal/server/reply.go:107; ignored temperature: D:/kiro-go/internal/server/messages.go:146.
6. Transport deadlines: D:/kiro-go/internal/server/timeouts.go:47; first-body-byte deadline: D:/kiro-go/internal/server/messages.go:452; regression: D:/kiro-go/internal/server/stability_test.go:91.
7. Native pin: D:/kiro-go/internal/server/messages.go:165; stable system: D:/kiro-go/internal/server/thread.go:33; trace: D:/kiro-go/internal/server/trace.go:241.
8. Failure ratio: D:/kiro-go/internal/pool/pool.go:893; atomic sidecar: D:/kiro-go/internal/pool/pool.go:438; last-account guard: D:/kiro-go/internal/pool/pool.go:522; rollback: D:/kiro-go/internal/pool/pool.go:878; restart regression: D:/kiro-go/internal/pool/priority_test.go:243.
9. Suspicious classification: D:/kiro-go/internal/kiro/failure.go:89; soft-ban cooldown: D:/kiro-go/internal/pool/pool.go:1005.
10. Listen guard: D:/kiro-go/cmd/kiro-proxy/main.go:207; HTTP deadlines: D:/kiro-go/cmd/kiro-proxy/main.go:185; bounded trace: D:/kiro-go/internal/server/trace.go:241; safe logs: D:/kiro-go/internal/kiro/redact.go:44.
