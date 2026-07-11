# dbferry

> Ships your databases to your own bucket. Every night. One by one.

Per-database backups for managed PostgreSQL and MySQL — streamed, compressed, encrypted, delivered to your own S3-compatible storage.

**Status: pre-release.** Nothing to see here yet.

## Why

Managed database providers back up your *cluster*. Restoring means restoring everything — every database, every tenant, to the same point in time. dbferry dumps each database separately, so you can restore one without touching the rest.

- `dbferry run --dsn postgres://... --dest s3://your-bucket/prefix`
- Streams `dump | zstd | age | s3` — no intermediate disk, client-side encryption, your key
- Auto-discovers every database on the cluster
- Any S3-compatible destination

## Docs

Architecture decision records live in [`docs/adr`](docs/adr).

## License

TBD (open-core: this CLI will be free forever).
