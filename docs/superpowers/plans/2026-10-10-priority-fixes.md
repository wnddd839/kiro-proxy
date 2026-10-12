# Priority Fixes Implementation Plan

**Goal:** Repair the ten reported issues, validating and reporting each group before the next.

**Architecture:** Keep the existing protocol-independent dispatch, cache simulator, journal and pool. Separate physical hidden context from client-visible prompt tokens and from billed hidden context. Reuse the existing locked reservation table. Preserve actual settlement separately from monotonic streaming estimates.

**Constraints:** No git staging or commits. No changes to bin/accounts.json, data or key files. No live account probing. Opus 5.5 remains 4052 and uncalibrated.

## Group 1: Metering and budgets
- [x] Add failing calibration, missing-price, budget and HTTP usage regression tests.
- [x] Add hidden billing configuration, model overrides, separate usage/journal fields and model calibration status.
- [x] Calibrate only client input categories, leaving output and reasoning unchanged; never assign rounding residual to uncached input.
- [x] Emit conservative initial stream estimates, monotonic final display and explicit actual settlement when different.
- [x] Resolve incomplete online prices from builtins; unknown models use the highest rate or configured rejection, with warnings.
- [x] Include this request in locked budget admission; settle/release atomically and test concurrency.
- [x] Run gofmt on changed Go files, go vet ./..., go test -race ./...; report eight-round usage comparison and changed files.

## Group 2: Protocol and stability
- [x] Add failing HTTP regressions for output limits, cross-event stop matching and hung upstreams.
- [x] Limit delivered text/thinking/tool output locally and meter only delivery; log unsupported temperature as ignored.
- [x] Bound connection, first response and idle stream time; cancel/close before joining readers, release leases/reservations.
- [x] Pin native effort and stable thread prefix; record thread-key changes and redacted prefix diagnostics.
- [x] Run gofmt, go vet ./..., go test -race ./... and report before group 3.

## Group 3: Pool and security
- [x] Add failing window/single-account/persistence, suspicious-429 and startup-security regressions.
- [x] Keep failure history through success, roll back strikes at breaker activation and protect the last account.
- [x] Atomically persist non-secret cooldown/statistics separately from account credentials; discard expired cooldowns.
- [x] Treat suspicious-activity 429 as a 24-hour soft ban.
- [x] Refuse unauthenticated wildcard startup unless explicitly overridden; bound HTTP reads and debug trace storage/retention.
- [x] Verify logs and debug traces are redacted; run gofmt, go vet ./..., go test -race ./... and report all ten statuses.

## Acceptance limitations
- Issue 2: initial stream usage is a local estimate, not a mathematically guaranteed lower bound of arbitrary future upstream reports; monotonic display and explicit settled_usage are implemented.
- Issue 4: concurrent reservation and settlement are implemented, but reported upstream input/credits can exceed local/history estimates; this is partial mitigation, not a hard spending ceiling.
- Opus 5.5 remains 4052 and uncalibrated; no live probe was run.
