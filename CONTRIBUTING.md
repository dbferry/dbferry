# Contributing to dbferry

Thanks for your interest! dbferry is pre-release and moving fast, so before
building anything sizable, please open an issue first — it may already be in
motion, or deliberately out of scope (see the ADRs).

## Development setup

You need Go 1.26+ and, for the integration suite, Docker plus database client
tools (`pg_dump`/`pg_restore` ≥ 17, `mysqldump`, `mysql`).

```sh
make build          # compile
make test           # unit suite — fast, no external services
make test-race      # unit suite under the race detector
make vet fmt        # baseline hygiene

make stand-up       # start the integration stand (PG 14 + 17, MySQL 8, MinIO)
make test-integration   # backup → restore → compare + CLI suite
make test-fault     # fault-injection suite
make cover          # coverage across all suites, threshold-enforced
make stand-down     # stop the stand and drop its data
```

CI runs exactly these checks — `gofmt`, `go vet`, unit tests with `-race`, the
integration and fault suites against the stand, and the coverage threshold.

## Ground rules

- **Architecture decisions live in [`docs/adr/`](docs/adr)** and are accepted,
  not open questions. If you want to change one, propose a new ADR — don't
  reverse a decision silently in code.
- **The backup format is a public contract** (ADR-0005). Layout or key-schema
  changes require a new schema version, never a silent edit.
- **Both engines are first-class**: new pipeline behavior goes through the
  `DatabaseDriver` interface — don't hardcode Postgres assumptions.
- **CLI UX**: human-readable output by default, `--json`/`--quiet`/`--no-color`
  where they make sense; errors state cause, impact, and a concrete fix; no
  fake progress percentages.
- **Never commit secrets** — no credentials, DSNs with passwords, age secret
  keys, or `.env` files, even in tests. The stand generates throwaway
  identities into a gitignored directory.
- Code and docs are in English; run `gofmt` before pushing.

## Reporting bugs

Open a GitHub issue with your dbferry version, the command you ran (with
credentials redacted — dbferry redacts DSNs in its own output, but double-check
anything you paste), and what you expected vs. got.

**Security issues: do not open a public issue** — see [`SECURITY.md`](SECURITY.md)
for private reporting.

## License

By contributing, you agree that your contributions are licensed under the
[Apache License 2.0](LICENSE).
