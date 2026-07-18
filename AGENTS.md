# Repository Guidelines

## Project Structure & Module Organization

This public repository contains the dbferry CLI and core, a working Go module. Packages live at the repo root (there is no `internal/` here): `cmd/dbferry/` (CLI entry point, hand-rolled `flag` parsing), `config/` (named connections, DSN templates, secret storage), `pipeline/` (streaming backup pipeline, engine drivers, discovery, GFS retention/prune/listing — exported for the private control plane, not yet wired to a CLI command), `test/integration/` (docker stand), and accepted architecture decisions in `docs/adr/`. Treat `docs/adr/` as the architectural source of truth; add new numbered ADRs there instead of silently reversing existing decisions.

Keep the dump pipeline reusable by both `dbferry run` and the private control plane's River jobs (it imports this module). Do not copy material from the sibling `internal/` workspace into this repo.

## Build, Test, and Development Commands

Use the Makefile targets:

```sh
make test              # unit suite — fast, no external services
make build             # go build ./...
make stand-up          # docker stand: PostgreSQL 14/17, MySQL 8, MinIO + bucket
make test-integration  # full backup→restore→compare + CLI suite (-tags=integration)
make test-fault        # fault-injection suite (-tags=faultinjection)
make cover             # coverage across all suites, threshold-enforced
```

Run `make fmt` (gofmt) and `make vet` before committing Go changes.

## Coding Style & Naming Conventions

Use idiomatic Go formatted by `gofmt`. Prefer small packages with clear boundaries. Follow the ADRs (the UI/server decisions — HTMX, pgx + sqlc, service layer — apply to the private control plane that imports this module). Public docs and code comments should be written in English.

## Testing Guidelines

Use Go's standard `testing` package. Name test files `*_test.go`, test functions `TestXxx`, and prefer table-driven tests for command parsing, driver behavior, and pipeline stages. Integration tests that require PostgreSQL, MySQL, or S3-compatible storage are gated behind build tags (`-tags=integration`, `-tags=faultinjection`) and need the stand (`make stand-up`), so plain `go test ./...` stays predictable for contributors.

## Commit & Pull Request Guidelines

Use short imperative commit subjects such as `add dump pipeline ADR` or `wire dbferry run command`. PRs should explain the user-facing change, link any relevant issue or ADR, list verification commands run, and include screenshots for UI changes. Never include secrets, `.env` files, pricing, competitor notes, or private business material.
