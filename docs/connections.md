# Named connections & config

Instead of passing a DSN, a destination and a recipient on every run, save them
once as a **named connection** and back up by name:

```sh
dbferry init
dbferry run --connection prod --database shop
```

## The config file

Config lives at `$XDG_CONFIG_HOME/dbferry/config.toml` (default
`~/.config/dbferry/config.toml`), mode `0600`. Override with `--config PATH` or
`$DBFERRY_CONFIG`. It is written atomically and refuses to load if it is a
symlink, world/group-readable, or owned by another user.

It holds **no secret values** — only where to find them:

```toml
[connections.prod]
engine           = "postgres"
dsn              = "postgres://fnd_user@db.example:5432/defaultdb?sslmode=verify-full"
password         = "keyring:dbferry/prod"     # or "env:DBFERRY_PROD_PASS"
default_database = "shop"                      # else --database is required
destination      = "do-spaces"
age_recipient    = "age1..."

[destinations.do-spaces]
bucket     = "my-space"
prefix     = "backups"
endpoint   = "https://fra1.digitaloceanspaces.com"   # empty for AWS S3
region     = "fra1"
access_key = "env:DO_KEY"
secret_key = "env:DO_SECRET"
# session_token = "env:AWS_SESSION_TOKEN"      # for STS/temporary credentials
# profile      = "..."                          # or an AWS shared-config profile
# (omit all three → the standard AWS credential chain is used)
```

A **connection is a cluster** — the stored DSN keeps all its options (TLS certs,
timeouts, query parameters) verbatim, with only the password removed. Which
database you back up is chosen per run: `--database` wins, otherwise
`default_database`; with neither, the run stops and asks. Backing up *every*
database in one command is out of scope for now.

## Secrets: keychain or environment

Secrets are referenced, never stored:

- `keyring:NAME` — the value lives in the OS keychain (macOS Keychain, Linux
  Secret Service, Windows Credential Manager). `init` puts it there. Best on
  interactive machines.
- `env:NAME` — the value is read from an environment variable at run time. Use
  this on headless servers/CI where no keychain is available.

`dbferry connections show` and `destinations show` print the *reference*
(`keyring:dbferry/prod`), never the value. Every resolved secret is scrubbed
from all output before anything is printed. Full rationale: ADR-0004.

## Managing entries

```sh
dbferry connections  list
dbferry connections  add prod --dsn 'postgres://u:pass@h/db' --password-keyring dbferry/prod --destination do-spaces --age-recipient age1...
dbferry connections  show prod
dbferry connections  rm prod            # also removes its keychain secret if unused elsewhere

dbferry destinations add do-spaces --bucket my-space --endpoint https://fra1.digitaloceanspaces.com --region fra1 --access-key-env DO_KEY --secret-key-env DO_SECRET
```

Flags override a connection's defaults, e.g.
`dbferry run --connection prod --dest s3://other/prefix`.

## Verify before you rely on it

```sh
dbferry doctor --connection prod
```

checks: the database is reachable, a compatible dump client is present, the
role can read, and the destination allows write (required), read (recommended)
and delete (optional — an append-only bucket is fine). Each problem comes with
the exact grant, policy or install to fix it. `--json` for scripts.
