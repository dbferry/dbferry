# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

dbferry — per-database backups for managed PostgreSQL/MySQL. Each database is dumped separately and streamed (`dump | zstd | age | S3 multipart`) to the customer's own S3-compatible bucket, with no intermediate disk and client-side encryption (the customer holds the key). The value over managed-provider backups: providers only restore the whole cluster; dbferry lets you restore one database without touching the rest.

**Status: working CLI (pre-release).** The pipeline is proven end-to-end against real managed databases and real S3-compatible storage. Layout:

- `cmd/dbferry/` — the CLI (hand-rolled `flag` parsing + a command switch, no framework): `init`, `run`, `databases`, `test-connection`, `doctor`, `connections`/`destinations`, `keygen`.
- `config/` — named connections and destinations: TOML config, password-free DSN templates (`BuildDSN`/`ValidateDSNTemplate`), secrets via OS keyring or env refs (ADR-0004), redaction.
- `pipeline/` — the streaming pipeline, `DatabaseDriver` implementations (Postgres 14–18, MySQL), discovery, preflight, diagnostics, manifests/object keys (ADR-0005), and the GFS retention machinery (`ListBackups` / `SelectRetention` / `Prune`) — currently exported for the cloud server only; a `dbferry prune` CLI command is planned but not wired yet.
- `test/integration/` — the docker stand (PostgreSQL 14 + 17, MySQL 8, MinIO + bucket, throwaway age identity).

## Commands

- `make build` / `make vet` / `make fmt` — baseline checks
- `make test` / `make test-race` — unit suite (fast, no external services)
- `make stand-up` / `make stand-down` — start/stop the integration stand
- `make test-integration` — full backup→restore→compare + CLI suite against the stand (`-tags=integration`)
- `make test-fault` — fault-injection suite against the stand (`-tags=faultinjection`)
- `make cover` — coverage across all suites, threshold-enforced (needs the stand)

## This is the public open-core repo

This git repository is `dbferry/dbferry` — **public**. It ships the free-forever CLI and core. A sibling `internal/` directory (business/pricing/positioning/competitor docs, private forever) lives *outside* this repo and its git history.

**Never add business, pricing, competitor, or positioning material to this repo, and never copy anything from `internal/` into it** — git never forgets, and this history is public. No secrets (credentials, keys, `.env`) anywhere. Public-facing docs and code here are written in English.

## Locked architecture decisions

Read `docs/adr/` before proposing alternatives — these are accepted, not open questions:

- **One Go binary for everything** (ADR-0001): control plane, scheduler, and workers in a single static binary (~20MB image, runs on the cheapest VPS). Job queue and periodic jobs: **River on PostgreSQL** — one Postgres holds all state, no Redis, no external cron. DB access: **pgx + sqlc, no ORM**. The streaming pipeline is composed readers: `os/exec` + `io.Pipe` + S3 SDK multipart upload.
- **Ship the pipeline as a standalone CLI first**: the dump pipeline is its own package driven by `dbferry run --dsn ... --dest s3://...`; the *same package* is later invoked from River jobs unchanged. Keep it decoupled from the control plane.
- **Server-rendered UI** (ADR-0002): `html/template` + HTMX, Tailwind via the standalone CLI (no Node on the server), all assets embedded via `embed.FS` so the binary stays self-contained. Live fragments via HTMX (e.g. `hx-trigger="every 5s"` for running-backup status). Auth: magic link + sessions, no passwords.
- **Handlers call a service layer, never talk to the DB directly.** That service layer is designed to later back a JSON API — the HTMX UI is a replaceable shell, so keep business logic out of handlers and templates.
- **Both engines from v1**: Postgres and MySQL behind a `DatabaseDriver` interface (listDatabases / buildDumpCommand / buildRestoreCommand / testConnection). Don't hardcode Postgres assumptions.
- **The backup format is a public contract** (ADR-0005): `pg_dump -Fc -Z0` (our zstd owns compression), versioned object key schema (`key_schema: 1` in the manifest), a backup is valid only with its manifest (written only after the multipart upload completes), multipart defaults 32 MiB parts × concurrency 4, BYOK age encryption. Layout changes require a new schema version, never a silent edit.

## Conventions

- ADRs are the source of architectural truth; add a new one in `docs/adr/` rather than reversing a decision silently.
- CLI UX: human-readable output by default with `--json` / `--quiet` / `--no-color` flags; honest progress stages (no fake percentages); errors that state cause + impact + concrete fix; destructive actions require explicit confirmation (`--yes` for automation); `--dry-run` for restore plans.
- Container images publish to `ghcr.io/dbferry/*`.
