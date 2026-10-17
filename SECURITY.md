# Security policy

## Reporting a vulnerability

Do not open a public issue for a vulnerability that could:

- expose account credentials, refresh tokens, API keys, or usage data
- let a remote request reach the account pool without authentication
- escape the admin loopback/token boundary
- cause the proxy to send data to an unintended host

Use GitHub's private vulnerability reporting on the repository's **Security**
tab. If that is unavailable, contact the maintainer through the profile linked
in the repository.

Include:

- affected version or commit
- deployment mode (binary, Docker, reverse proxy)
- a minimal reproduction
- impact and any prerequisites

You will get an acknowledgement when the report is seen. There is no bug bounty.

## Supported versions

Only the latest release is supported. Security fixes are shipped in a new
release rather than backported to older tags.

## Security model

- The listener defaults to `127.0.0.1:8787`.
- A non-loopback listener without a downstream key is refused unless the
  operator passes `-allow-insecure-listen`.
- `/admin` accepts loopback requests only, unless `admin_token` is set.
  Forwarding headers disqualify a request from being treated as local.
- Downstream keys are stored as SHA-256 hashes. Plaintext is shown once.
- Account credentials live in `accounts.json` with mode `0600`.
- Logs pass through a sanitizing handler. Debug snapshots are off by default and
  redact credentials, profile ARNs, and image bytes, but still contain
  conversation text.
- The container runs as a non-root user in a distroless image.

## Out of scope

- Risks from violating Kiro or AWS terms of service. The project documents that
  risk and does not provide evasion features.
- Account suspension caused by upstream enforcement.
- Secrets placed in prompts, issue reports, or screenshots by the user.
- Vulnerabilities in clients or reverse proxies configured around this project.
