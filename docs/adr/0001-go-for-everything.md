# ADR-0001: Go for the entire product

Date: 2026-07-11
Status: accepted

## Context

The product core is process orchestration: spawning `pg_dump`/`mysqldump`, streaming through compression and encryption into S3 multipart uploads, with retries, timeouts, and cancellation. A control plane (UI, schedules, credentials) sits on top. The initial candidate stack was Laravel + Filament (fast CRUD), with Go workers later.

## Decision

Single Go codebase for everything: control plane, scheduler, and workers in one static binary.

- Job queue and periodic jobs: River on top of PostgreSQL — one Postgres holds all state, no Redis, no external cron
- DB access: pgx + sqlc, no ORM
- Streaming pipeline as composed readers: `os/exec` + `io.Pipe` + S3 SDK multipart upload
- UI: html/template + HTMX, assets via embed.FS (see ADR-0002)

## Consequences

- Deploy = one binary or a ~20MB image; runs comfortably on the cheapest VPS
- No PHP-FPM/runtime tuning; idiomatic contexts/cancellation for long-running dumps
- We give up Filament's free CRUD; UI is hand-built (acceptable: 5 screens, and the moat is the pipeline, not the dashboard)
- The dump pipeline ships first as a standalone CLI package — the same package is later invoked from River jobs unchanged
