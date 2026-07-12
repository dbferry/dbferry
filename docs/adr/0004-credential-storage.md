# ADR-0004: Credential storage for named connections

Date: 2026-07-12
Status: accepted

## Context

The CLI is gaining named connections (`dbferry init` → `dbferry run
--connection prod`) so users stop passing DSN files and flags on every run.
That requires somewhere to keep connection details and, crucially, secrets: the
database password and the S3 access/secret/session keys. The same config and
secret model will later back the cloud service, so the decision is not
throwaway.

Constraints:

- **No plaintext secrets on disk.** A config file is not a secret store; a
  leaked or backed-up dotfile must not expose credentials.
- Must work both on interactive dev machines and on headless servers/CI (cron
  backups), where an OS keychain / D-Bus Secret Service is often absent.
- Must preserve arbitrary, provider-specific DSN options (TLS cert paths,
  `connect_timeout`, MySQL `tls`/`parseTime`, unix sockets, percent-encoded
  values) without weakening them — a fixed set of host/port/user fields cannot.

## Decision

- **Config**: TOML at `$XDG_CONFIG_HOME/dbferry/config.toml` (default
  `~/.config/dbferry/config.toml`), mode `0600`, written atomically (temp file +
  `fsync` + `rename`, under a file lock). It holds only non-secret data and
  **secret references**, never secret values.
- **Secrets** are addressed by a `SecretRef` with two providers:
  - `keyring` — the OS keychain (macOS Keychain, Linux Secret Service, Windows
    Credential Manager) via `github.com/zalando/go-keyring`. The default on
    interactive machines; `init` writes the secret here and the config stores
    `{ keyring = "dbferry/prod" }`.
  - `env` — an environment variable name (`{ env = "DBFERRY_PROD_PASS" }`),
    resolved at run time. The fallback for headless servers/CI where no keychain
    exists.
  A keyring ref on a machine without a keychain is a clear, actionable error
  (set the env var, or run where a keychain agent is available), never a silent
  failure.
- **Connections store a DSN template, not decomposed fields**: the full DSN with
  the password removed (`postgres://user@host:port/db?sslmode=verify-full&sslrootcert=…`)
  plus the engine and the password `SecretRef`. At run time the password is
  injected via `url.UserPassword` (correct percent-encoding) and, for a
  per-database backup, the database path is swapped. All other options survive
  verbatim. Secrets are forbidden inside the stored query string.
- **Redaction**: all resolved secrets (DB password, S3 access/secret/session
  token) are collected into a single redaction set and scrubbed from every
  stdout/stderr/log line before any network call.

## Consequences

- A leaked `config.toml` exposes no credentials — only where to find them.
- Interactive users get "it just works" via the keychain; servers/CI use env
  references; both are first-class, selected per secret.
- Arbitrary DSN options (TLS, timeouts, provider quirks) are preserved, so we
  don't silently break or downgrade a working connection. The cost: engine
  drivers must round-trip those options to both the DB client and the external
  dump tool (e.g. map `sslrootcert` → `PGSSLROOTCERT` for pg_dump), which the
  PoC drivers do not yet fully do — tracked in the phase plan (0.5.2).
- The keyring dependency is platform-specific; on unsupported/headless
  environments we degrade to env references rather than fail.
- Secret lifecycle (delete on `connections rm` unless shared, rollback keychain
  write if config write fails) must be handled explicitly; specified in the
  phase plan (0.5.2).
