# dbferry

[![ci](https://github.com/dbferry/dbferry/actions/workflows/ci.yml/badge.svg)](https://github.com/dbferry/dbferry/actions/workflows/ci.yml)

> Ships your databases to your own bucket. Every night. One by one.

Per-database backups for managed PostgreSQL and MySQL — streamed, compressed,
encrypted, delivered to your own S3-compatible storage.

**Status: pre-release.**

## Why

Managed database providers back up your *cluster*. Restoring means restoring
everything — every database, every tenant, to the same point in time. dbferry
dumps each database separately, so you can restore one without touching the rest.

- Streams `dump | zstd | age | s3` — no intermediate disk, client-side
  encryption, **your** key (dbferry never holds it: zero data retention)
- PostgreSQL and MySQL; any S3-compatible destination (AWS S3, DigitalOcean
  Spaces, Cloudflare R2, Backblaze B2, MinIO)
- Auto-discovers every database on the cluster; picks a matching `pg_dump`
  (PostgreSQL 14–18 supported)

## Install

Prebuilt binaries for Linux, macOS and Windows (amd64/arm64) are on the
[releases page](https://github.com/dbferry/dbferry/releases), with sha256
checksums. Or build from source with Go 1.26+:

```sh
go install github.com/dbferry/dbferry/cmd/dbferry@latest
```

The container image bundles `pg_dump` 14–18 and the MySQL client
(linux/amd64):

```sh
docker pull ghcr.io/dbferry/dbferry:latest
```

### Requirements

- **PostgreSQL sources**: `pg_dump` / `pg_restore` with a major version ≥ your
  server's (14–18). dbferry checks Debian's versioned layout
  (`/usr/lib/postgresql/<major>/bin`) first, then `PATH`, and picks a matching
  client per server — `dbferry doctor` tells you if yours won't do.
- **MySQL sources**: `mysqldump` on `PATH`.
- Compression (zstd) and encryption (age) are built into the binary — nothing
  else to install for backups. Restoring uses the standard `age` and `zstd`
  CLI tools (see [Restore](#restore)).

## Quickstart

One-time setup, then back up by name:

```sh
dbferry init                      # wizard: connection → destination → key, all verified
dbferry run --connection prod --database shop
```

Or drive it directly, no config:

```sh
export DBFERRY_DSN='postgres://user:pass@host:5432/shop'   # never passed on argv
dbferry run --dest s3://my-bucket/prefix --age-recipient age1...
```

Create your encryption key (keep the private half safe — losing it loses your
backups):

```sh
dbferry keygen --out ~/dbferry-identity.txt   # prints the age recipient to use
```

Check everything before you rely on it:

```sh
dbferry doctor --connection prod   # connect, dump-client match, bucket write/read/delete
```

## Commands

| | |
| --- | --- |
| `dbferry init` | interactive setup of a named connection |
| `dbferry run` | back up one database (`--connection NAME` or `--dsn-* --dest`) |
| `dbferry databases` | list the databases on a cluster |
| `dbferry test-connection` | verify a connection without backing up |
| `dbferry doctor` | diagnose source and destination with concrete fixes |
| `dbferry connections` / `destinations` | manage named entries (`list/show/add/rm`) |
| `dbferry keygen` | generate an age identity |

`--json` / `--quiet` / `--no-color` on the read-and-report commands. Exit codes:
`0` success, `3` connect, `4` dump, `5` upload, `130` canceled.

## Restore

Restore is the inverse chain, documented step by step (download → `age -d` →
`zstd -d` → `pg_restore`/`mysql`) in [`docs/restore.md`](docs/restore.md).

## Docs

- [Named connections & config](docs/connections.md) — the config file and how
  secrets are stored (never in plaintext)
- [Restoring a backup](docs/restore.md)
- [Operating backups](docs/operations.md) — bucket lifecycle for incomplete uploads
- [Architecture decisions](docs/adr)

## Contributing

Issues and PRs welcome — see [`CONTRIBUTING.md`](CONTRIBUTING.md) for the dev
setup (one `make stand-up` gives you real PG 14/17, MySQL 8 and MinIO to test
against) and the ground rules. It's pre-release: open an issue before building
anything sizable.

## Security

Found a vulnerability? Please report it privately — see [`SECURITY.md`](SECURITY.md).
Backups are encrypted client-side with your own age key; dbferry never holds it.

## License

[Apache License 2.0](LICENSE) — free forever. dbferry is open-core: this CLI and
the backup pipeline are open source; the hosted control plane that schedules and
manages them is a separate commercial product.
