# ARTEX (FailSafe build, English translation)

<p align="center">
  <img src="assets/artex-failsafe-header.png" alt="ARTEX — a source-built, English-translated distribution" width="100%">
</p>

A source-built, English-translated distribution of [ARTEX](https://github.com/RuoJi6/ARTEX),
an open-source multi-agent security testing harness, based on the
[FailSafe hardened build](https://github.com/failsafe-security/artex).

**What this fork adds on top of FailSafe's build:** the backend (Go), CLI
scripts, and web frontend were almost entirely in Chinese (comments and
user-facing strings). This fork translates all of it to English, with no
logic changes — same behavior, same hardening, readable by English-speaking
reviewers and contributors.

> **Authorized testing only.** ARTEX runs an autonomous agent that can execute
> shell commands and send network traffic. Use it only against systems you own
> or have explicit written permission to test.

## Why the FailSafe build specifically

In November 2026, public analysis found that prebuilt Docker images published
under `tomicdd/artex` (and copies under `congdonglinux/artex`) contained an
unauthenticated command-execution backdoor in `server/auth.go` — a hidden
`verifyCaptcha` function that ran arbitrary shell commands supplied in the
login request, before checking credentials. The backdoor was image-only; the
GitHub source did not contain it. FailSafe's fork responds to that by building
from source and never pulling an ARTEX image, with digest-pinned base images.
This repository builds on that same source-built foundation. See
[`UPSTREAM.md`](UPSTREAM.md) and
[`audit/artex-v0.3.15-audit.md`](audit/artex-v0.3.15-audit.md) for the full
provenance and audit trail.

## Quick start

Requirements: Docker Engine with Compose v2, and about 5 GB of free disk
(the browser stack and Chromium account for most of the image size).
If your Docker installation only provides the standalone `docker-compose`
command, substitute it for `docker compose` below; `make` detects either.

```bash
cp .env.safe.example .env.safe
chmod 600 .env.safe
# edit .env.safe: set POSTGRES_PASSWORD and, optionally, an LLM key
docker compose --env-file .env.safe -f docker-compose.safe.yml up -d --build
```

Open <http://127.0.0.1:8787> and complete the first-run setup. If you left the
LLM key blank, configure a provider in the console before starting a scan.

Check status and logs:

```bash
docker compose --env-file .env.safe -f docker-compose.safe.yml ps
docker compose --env-file .env.safe -f docker-compose.safe.yml logs -f artex
```

Stop without deleting data:

```bash
docker compose --env-file .env.safe -f docker-compose.safe.yml down
```

Delete everything, including the database and ARTEX data:

```bash
docker compose --env-file .env.safe -f docker-compose.safe.yml down -v
```

## What is different from upstream ARTEX

| Area | Upstream default | This build |
|---|---|---|
| Image | prebuilt `tomicdd/artex` (backdoored) | compiled from this source tree |
| Language | Chinese comments/strings throughout | translated to English |
| Runtime user | root | root (upstream parity) |
| Console binding | `0.0.0.0:8787` | `127.0.0.1:8787` |
| Recording proxy | published on `8788` | not published |
| Browser automation | Node 22 + Playwright + Chromium | same, pinned versions |
| Browser MCP launcher | `npx @playwright/mcp` | pinned `playwright-mcp` |
| Capabilities | default Docker set | default Docker set + `no-new-privileges` |
| Self-update | points at upstream | compiled off; no remote check or download |
| Base images | floating tags | pinned by digest |

The API, UI, skills, browser MCP, and browser-driven recon are functionally
unchanged from the FailSafe build. Node, Playwright, the Playwright MCP/CLI,
and Chromium are preinstalled; `nmap` raw scans and runtime `apt`/`npm -g`
installs work because the container runs as root, as upstream does. The
deployment hardening that remains is the loopback-only console, an
unpublished proxy, `no-new-privileges`, a persisted JWT key, and
digest-pinned base images.

## Build without Docker

```bash
cd web
npm ci --ignore-scripts
npm run build:static
cd ..

rm -rf server/webui/dist
mkdir -p server/webui/dist
cp -R web/out/. server/webui/dist/

CGO_ENABLED=0 go build \
  -trimpath -buildvcs=false -tags embedui \
  -ldflags="-buildid= -X main.version=v0.3.15-failsafe.2" \
  -o artex ./cmd/artex
```

Then run `./artex -addr 127.0.0.1:8787 -proxy 127.0.0.1:8788` with
`ARTEX_PG_DSN` set to a PostgreSQL database you control. This bare-binary path
does not install Node, Playwright, Chromium, or the recon CLI tools; use the
Docker image for full upstream capability.

## Verify the build

```bash
make verify
```

This checks the known implant indicators (`verifyCaptcha`, `tomicdd`,
`congdonglinux`), prints the binary SHA-256, and lists the image ID. See
[`audit/artex-v0.3.15-audit.md`](audit/artex-v0.3.15-audit.md) for the full
audit of this tag.

## Data and secrets

- `.env.safe` contains the database password and optional LLM keys. Keep it
  `0600` and out of version control.
- The database lives in the `pgdata` volume; ARTEX data and evidence live in
  `artex_data`; the JWT signing key lives in `artex_key`. Back up all three.
- ARTEX stores scan evidence, which may include credentials, tokens, and
  customer data. Treat `/app/data` and the database as sensitive.
- For a backup, stop the stack and archive the volumes, or use
  `pg_dump` inside the `postgres` container.

## Updating

Read [`UPSTREAM.md`](UPSTREAM.md) first. In short: review the upstream diff,
rebuild, run `make verify`, update the audit report, and only then deploy.
This repository tracks [`failsafe-security/artex`](https://github.com/failsafe-security/artex)
for hardening and security fixes, and re-applies the translation on top of
each pull.

## License and provenance

ARTEX is AGPL-3.0. See [`LICENSE`](LICENSE) and
[`UPSTREAM.md`](UPSTREAM.md). This fork's modifications (the English
translation) are released under the same license. The upstream README is
preserved as [`README.upstream.md`](README.upstream.md).
