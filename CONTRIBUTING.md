# Contributing

Thanks for taking the time to report a problem or send a change.

This project is maintained by one person. The fastest way to get a useful
response is a small, concrete report or patch.

## Issues

Use the issue templates:

- **Bug report** for incorrect behavior.
- **Cache hit problem** for low cache-read share or a regression.
- **Feature request** for new capabilities.

Include the version, platform, client, protocol, and the console request row.
Redact tokens, API keys, account emails, and conversation content.

## Pull requests

1. Open an issue first for anything larger than a typo or a small fix. It avoids
   work that does not fit the project's direction.
2. Keep the change focused. One behavior change per pull request.
3. Add or update tests. The repository expects `go test -race ./...` to pass.
4. Run `make check` before pushing. It runs gofmt, vet, and the race test suite.
5. Match the surrounding style. Comments and docs are in Chinese in some
   packages and English in others; follow the file.
6. Do not commit runtime data: `accounts.json`, `keys.json`, `usage.jsonl`,
   `promptcache.json`, `prices.json`, `.env`, or `kiro-proxy.json`.

## Development setup

```sh
git clone https://github.com/wnddd839/kiro.cache.git
cd kiro.cache

make check
make build
```

Run locally:

```sh
bin/kiro-proxy -config kiro-proxy.json
```

Create a config file only when you need non-default behavior. A missing file
means defaults.

## Project boundaries

The project deliberately does not:

- Store model answers locally.
- Share cache or answers across accounts.
- Add per-account proxy or fingerprint evasion.
- Resell access or manage accounts for other people.
- Claim a cache hit without measurement.

Changes that cross these boundaries need a design discussion first.

## Commit messages

Use a short prefix that describes the area and the effect:

```text
fix: keep auth enabled after the last key is deleted
docs: split configuration into its own page
test: cover 1h cache renewal
```

Explain why in the body when the change is not obvious.

## Security

Do not open a public issue for a vulnerability that could expose credentials or
let a remote request reach the pool. Send a private report through the
repository's Security tab, or contact the maintainer through the profile linked
in the repository.

## License

By contributing, you agree that your contribution is licensed under the
project's BSD 3-Clause license.
