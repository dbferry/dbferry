# Restoring a dbferry backup

Every dbferry backup is a **single database**, stored in your own bucket as two
objects that share one `backup_id`:

```
…/<engine>/<cluster>/<database>/<YYYY>/<MM>/<backup_id>.dump.zst.age    ← the backup
…/<engine>/<cluster>/<database>/<YYYY>/<MM>/<backup_id>.manifest.json   ← its manifest
```

The backup is produced by a streaming `dump | zstd | age` pipeline, so restoring
is always the inverse chain: **download → age-decrypt → zstd-decompress → load**.
The `engine` and `format` fields in the manifest tell you which dump tool made
it — PostgreSQL (`pg_dump`) is covered first below, MySQL (`mysqldump`) in its
own section. An object is only a valid backup if its `.manifest.json` exists
next to it; an object without a manifest is incomplete and must not be trusted.

> **You hold the key.** dbferry encrypts to your age *recipient* and never keeps
> your private key (zero data retention). You need your **age identity** to
> restore. If you lose it, the backup is unrecoverable — nobody, including us,
> can decrypt it. Keep the identity in a password manager and an offline copy.

## What you need

- Your **age identity file** (the `AGE-SECRET-KEY-1…` you generated with
  `age-keygen`). This is the private counterpart of the recipient you gave
  dbferry.
- [`age`](https://github.com/FiloSottile/age) (or `rage`).
- [`zstd`](https://github.com/facebook/zstd).
- PostgreSQL client tools (`pg_restore`, `createdb`, `psql`) with a **major
  version ≥ the client that produced the dump** — see `dump_client` in the
  manifest.
- An S3 client: the `aws` CLI (or `mc`). For non-AWS S3-compatible storage
  (MinIO, Backblaze B2, Cloudflare R2, …) add `--endpoint-url`.

Set the variables used below to your own values:

```sh
BUCKET=my-backups
KEY=postgres/db.example_5432/shop/2026/07/20260711T164025Z-01KX90TDX1XVMNBKV3GTF67CE3
IDENTITY=~/secrets/dbferry-identity.txt          # your age secret key
```

## 1. Find the backup

List the backups for a database (each one is an object + a manifest):

```sh
aws s3 ls --recursive "s3://$BUCKET/postgres/db.example_5432/shop/"
```

## 2. Verify integrity (recommended)

The manifest records the size and SHA-256 of the ciphertext. Confirm the object
in the bucket matches before you restore:

```sh
aws s3 cp "s3://$BUCKET/$KEY.manifest.json" -            # read the manifest
aws s3 cp "s3://$BUCKET/$KEY.dump.zst.age" - | sha256sum  # compare to ciphertext_sha256
```

## 3. Decrypt, decompress, and restore

Stream the object through decryption and decompression into a local dump file:

```sh
aws s3 cp "s3://$BUCKET/$KEY.dump.zst.age" - \
  | age -d -i "$IDENTITY" \
  | zstd -d \
  > shop.dump
```

`shop.dump` is a standard PostgreSQL custom-format archive. Restore it into a
**fresh, empty database**:

```sh
createdb -h "$PGHOST" -U "$PGUSER" shop_restored
pg_restore -h "$PGHOST" -U "$PGUSER" -d shop_restored --no-owner shop.dump
```

Notes:

- The dump is **custom format** (`-Fc`): restore with `pg_restore`, not `psql`.
- Restore into an **empty** database. `pg_restore` does not drop existing
  objects unless you pass `--clean`.
- `--no-owner` restores objects under the connecting role instead of the
  original owner; drop it if you want to preserve ownership and have the roles.
- **Parallel restore:** `pg_restore -j 4 …` is much faster on large dumps. It
  needs the `shop.dump` **file** (a pipe is not seekable), which is why step 3
  writes to disk rather than piping straight into `pg_restore`.

### Restore a single table

This is dbferry's core value — you do not have to restore the whole database.
Custom-format archives are selective:

```sh
pg_restore -l shop.dump                    # list the archive's contents (TOC)
pg_restore -h "$PGHOST" -U "$PGUSER" -d shop_restored \
  --no-owner -t orders shop.dump           # restore only the "orders" table
```

For finer control, edit the TOC (`pg_restore -l … > toc; …edit…`) and restore
with `-L toc`.

## MySQL

MySQL backups are made with `mysqldump --single-transaction --set-gtid-purged=OFF
--routines --events` (triggers travel with their tables), so the decrypted
artifact is a **plain SQL script**, not a binary archive. Restore it by piping
into the `mysql` client.

You need the same age identity, `age`, `zstd`, the MySQL client tools (`mysql`),
and an S3 client. Set:

```sh
BUCKET=my-backups
KEY=mysql/db.example_3306/shop/2026/07/20260711T164025Z-01KX90TDX1XVMNBKV3GTF67CE3
IDENTITY=~/secrets/dbferry-identity.txt
```

Decrypt and decompress to a `.sql` file:

```sh
aws s3 cp "s3://$BUCKET/$KEY.dump.zst.age" - \
  | age -d -i "$IDENTITY" \
  | zstd -d \
  > shop.sql
```

The dump contains no `CREATE DATABASE`/`USE`, so create the target database and
load into it — restoring into whatever database name you choose:

```sh
mysql -h "$MYSQL_HOST" -u "$MYSQL_USER" -e "CREATE DATABASE shop_restored"
mysql -h "$MYSQL_HOST" -u "$MYSQL_USER" shop_restored < shop.sql
```

Notes:

- Put the password in the `MYSQL_PWD` environment variable rather than on the
  command line, so it doesn't land in your shell history or `ps`.
- Restore into an **empty** database. The dump recreates tables, indexes,
  triggers, stored routines (`--routines`) and events (`--events`).
- **Single table:** an SQL dump isn't randomly seekable, so there's no
  `pg_restore -t` equivalent. Restore the whole database into a scratch schema
  and copy the one table out (`INSERT ... SELECT` or `mysqldump scratch table`),
  or filter the `.sql` with `sed`/`awk` for that table's section.
- If the backup was taken with `--allow-nontransactional`, non-InnoDB tables
  (e.g. MyISAM) may be inconsistent if they were written during the backup.

## S3-compatible storage (MinIO, R2, B2, …)

Point the `aws` CLI at your endpoint and provide credentials via the standard
environment variables:

```sh
export AWS_ACCESS_KEY_ID=…  AWS_SECRET_ACCESS_KEY=…  AWS_REGION=us-east-1
aws --endpoint-url https://s3.example.com s3 cp "s3://$BUCKET/$KEY.dump.zst.age" - \
  | age -d -i "$IDENTITY" | zstd -d > shop.dump
```

## Troubleshooting

| Symptom | Cause / fix |
| --- | --- |
| `age: error: no identity matched any of the recipients` | Wrong identity file — this backup was encrypted to a different key. |
| `pg_restore: error: unsupported version … in file header` | Your `pg_restore` is older than the `dump_client` in the manifest. Upgrade the client tools. |
| `zstd: error … not in zstd format` | The stream was not decrypted first, or the object is truncated (check step 2). |
| SHA-256 does not match the manifest | The object is corrupt or was modified. Do not use it; take a fresh backup. |
