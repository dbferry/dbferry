# Repository Guidelines

## Project Structure & Module Organization

This public repository is currently pre-code. It contains `README.md`, accepted architecture decisions in `docs/adr/`, and contributor guidance. Treat `docs/adr/` as the architectural source of truth; add new numbered ADRs there instead of silently reversing existing decisions.

When implementation starts, keep the CLI entry point under `cmd/dbferry/` and app-only Go packages under `internal/`. Keep the dump pipeline in a package reusable by both `dbferry run --dsn ... --dest s3://...` and later River jobs. Do not copy material from the sibling `internal/` workspace into this repo.

## Build, Test, and Development Commands

No build, lint, or test commands exist yet because no Go module has landed. Once code is added, expected baseline commands are:

```sh
go test ./...
go build ./cmd/dbferry
gofmt -w ./cmd ./internal
```

Use `go test ./...` for the full test suite, `go build ./cmd/dbferry` to verify the CLI binary, and `gofmt` before committing Go changes. Add any future generated-code commands, such as `sqlc generate`, beside the code that requires them and document them here.

## Coding Style & Naming Conventions

Use idiomatic Go formatted by `gofmt`. Prefer small packages with clear boundaries. Follow the ADRs: pgx plus sqlc for database access, no ORM; `html/template` plus HTMX for the UI; Tailwind via the standalone CLI; assets embedded with `embed.FS`. Keep handlers thin and route business logic through services. Public docs and code comments should be written in English.

## Testing Guidelines

Use Go's standard `testing` package. Name test files `*_test.go`, test functions `TestXxx`, and prefer table-driven tests for command parsing, driver behavior, and pipeline stages. Integration tests that require PostgreSQL, MySQL, or S3-compatible storage should be explicitly marked or gated by environment variables so `go test ./...` stays predictable for contributors.

## Commit & Pull Request Guidelines

The Git history is minimal (`start project`), so use short imperative commit subjects such as `add dump pipeline ADR` or `wire dbferry run command`. PRs should explain the user-facing change, link any relevant issue or ADR, list verification commands run, and include screenshots for UI changes. Never include secrets, `.env` files, pricing, competitor notes, or private business material.
