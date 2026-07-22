# Security policy

## Reporting a vulnerability

Please report security issues **privately**, not in a public issue.

- Email **security@dbferry.io** (or hello@dbferry.io) with details and, if
  possible, a minimal reproduction.
- We aim to acknowledge within a few business days and will keep you updated
  as we investigate and fix.
- Please give us a reasonable window to release a fix before any public
  disclosure. We're happy to credit you.

## Scope and design notes

dbferry is a backup tool; the security model is deliberately simple:

- **Backups are encrypted client-side** with your own [age](https://age-encryption.org)
  key. dbferry (and the hosted service) only ever handle your **public** recipient
  — the private identity never leaves your control. This is what "zero data
  retention" means: we cannot read your backups.
- **Losing the age private key loses the backups.** There is no escrow and no
  recovery path by design. Keep the identity file safe and backed up separately.
- **Database credentials** are never passed on the command line (argv is world-
  readable); use the config store (OS keyring / env refs) or `DBFERRY_DSN`.
  See [`docs/connections.md`](docs/connections.md).
- **Least privilege**: a backup role needs only `CONNECT` + `SELECT` (plus schema
  `USAGE`); the `onboard/` package generates exactly those grants and a
  prefix-scoped S3 policy.

## Never commit secrets

This repository must never contain real credentials, private keys, or `.env`
files. `AGE-SECRET-KEY-…` values, DSNs with passwords, and cloud keys belong in
a secrets manager, not in git. The `.gitignore` blocks the common cases, but the
final check is yours before every commit.
