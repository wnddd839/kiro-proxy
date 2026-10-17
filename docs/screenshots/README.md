# Screenshots

The README leaves space for three production screenshots. Capture them at
1440x1000 and save as PNG:

| File | Page |
|---|---|
| `overview.png` | 概览, with a non-trivial hit rate and several days of trend |
| `billing.png` | 用量账单, month view with per-key and per-model tables |
| `requests.png` | 请求, a multi-turn session showing repeated `cache read` |

## Before publishing

- [ ] Replace account emails and labels with neutral names.
- [ ] Clear or mask the API key field. Never show a real key.
- [ ] Remove thread / conversation hashes only if they could identify a session.
- [ ] Crop out browser chrome and bookmarks.
- [ ] Use synthetic or non-sensitive conversation content. The pages do not show
      message bodies, but error text and model names can still be sensitive.
- [ ] Check that no reverse-proxy domain, internal hostname, or path is visible.

The screenshots are intentionally not generated from a fake API: a real
console with real cache numbers is the point. If the numbers are small, label
the capture as a sample rather than inflating it.
