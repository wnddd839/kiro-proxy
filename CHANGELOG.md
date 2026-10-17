# Changelog

## Unreleased

### Added

- Docker image published to GHCR for `linux/amd64` and `linux/arm64`.
- `docker-compose.yml` with a persistent volume and required secrets.
- Release workflow that builds six platform binaries, attaches
  `SHA256SUMS.txt`, and pushes a multi-arch image on `v*` tags.
- `-listen` and `-healthcheck` flags, plus `KIRO_ADMIN_TOKEN` and
  `KIRO_API_KEY` environment variables for container deployments.
- Issue templates for bugs, cache hit problems, and feature requests.

### Changed

- Module path is now `github.com/wnddd839/kiro.cache`, so `go install` works.
- The protocol reference directory was renamed from `vendor/` to `third_party/`
  so Go tooling no longer treats it as a module vendor directory.
- README rewritten as a short entry point. Configuration, deployment, API,
  cache, and operations moved to `docs/`.
- Technical measurement notes moved to `docs/notes/`.

### Removed

- Internal review, acceptance, and planning documents.
- Internal research notes and unrelated design documents from test fixtures.
- Repository-root test logs.

### Fixed

- `go list -m all` no longer fails because of the reference directory.

## v0.4.0

See [docs/RELEASE-v0.4.0.md](docs/RELEASE-v0.4.0.md).

## v0.3.0

See [docs/RELEASE-v0.3.0.md](docs/RELEASE-v0.3.0.md).

## v0.2.1

See [docs/RELEASE-v0.2.1.md](docs/RELEASE-v0.2.1.md).

## v0.2.0

See [docs/RELEASE-v0.2.0.md](docs/RELEASE-v0.2.0.md).

## v0.1.0

See [docs/RELEASE-v0.1.0.md](docs/RELEASE-v0.1.0.md).
