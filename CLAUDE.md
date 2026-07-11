# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

dbferry — per-database backups for managed PostgreSQL/MySQL. Each database is dumped separately and streamed (`dump | zstd | age | S3 multipart`) to the customer's own S3-compatible bucket, with no intermediate disk and client-side encryption (the customer holds the key). The value over managed-provider backups: providers only restore the whole cluster; dbferry lets you restore one database without touching the rest.

**Status: pre-code.** This repo currently contains only `README.md` and `docs/adr/`. No source, build, lint, or test commands exist yet. The intended toolchain (from the ADRs) is Go with `go build` / `go test`; introduce it when code lands.

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

## Conventions

- ADRs are the source of architectural truth; add a new one in `docs/adr/` rather than reversing a decision silently.
- CLI UX: human-readable output by default with `--json` / `--quiet` / `--no-color` flags; honest progress stages (no fake percentages); errors that state cause + impact + concrete fix; destructive actions require explicit confirmation (`--yes` for automation); `--dry-run` for restore plans.
- Container images publish to `ghcr.io/dbferry/*`.
