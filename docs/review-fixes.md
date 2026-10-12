# Review fixes

These changes apply to the workspace snapshot `d1c9526` on branch `review-fixes`.
The snapshot preserves the changes that were already present before this review.
Each implementation fix has a separate commit.

| Review finding | Commit | Result |
|---|---|---|
| Authentication opens after deleting the last key | `d7e554b` | Authentication stays required, including after restart; malformed key credentials fail to load. |
| Response/journal/account costs differ | `a351d8b` | Credits estimates retain their provenance and use one settlement across all three destinations. |
| Lifetime budgets expire with detailed records | `8b0533d` | Lifetime budget counters survive pruning and restart via a compact checkpoint in the journal. Retried attempts remain excluded. |
| Reservations use unrelated prices and stacked estimates | `3127afb` | Credits billing reserves credits prices; API billing reserves applicable context/cache prices. No doubled input or historical-average floor. |
| Invalid tool arguments become `{}` | `62c5adc` | Invalid or locally truncated JSON fails explicitly, for both streaming and collected responses. |
| Standard streaming usage differs from settlement | `a7f8a75` | Initial unknown usage is zero; standard final usage matches the journal. The legacy extension contains the same result. |
| Journal faults silently discard records | `2b1ae22` | Accepted records stay queued. Disk errors or the backlog threshold pause new generation and make health return 503; successful retry resumes service. |
| Last-account protection hides permanent failure | `3db969e` | Definite suspension/expired credentials disable the account while retaining its record. Temporary network failures still cool it. |
| Repeated logger wrappers and missing member names | `f2791e8` | SafeLogger is idempotent; member metadata uses `key_name`, while credential fields remain redacted. |

Validation passed:

```powershell
go test -race ./... -count=1 -timeout=180s
go vet ./...
go build ./...
git diff --check
```

Regression coverage uses local HTTP upstreams and temporary account/key/journal
files. It includes key deletion and replacement through the admin API, missing
credits in streaming/non-streaming responses, lifetime budgets across repeated
restarts, request-specific reservations, malformed/truncated tool arguments,
actual journal write failures and recovery, more than 32 MiB of accepted backlog,
and suspension of the only account.

Operational limits:

- Upstream token/credit use can exceed estimates; reservations are not a hard
  upstream spending cap.
- Old history already discarded by earlier versions cannot be reconstructed.
  Existing history still present in the journal migrates into lifetime counters.
- Journal writes remain asynchronous. Pending records require successful flush
  for restart durability. Already admitted completions can temporarily exceed
  the backlog admission threshold.
- No live upstream accounts were probed and no running service was restarted.
