# ADR-0005: Backup object format, key schema and validity invariant

Date: 2026-07-11 (decided at project start; recorded as an ADR 2026-07-18 —
code comments previously cited an internal decision log)

Status: accepted

## Context

Every backup dbferry produces lands in the customer's own bucket and must stay
readable years later, without dbferry running: the object layout and format are
a public contract with the customer, not an implementation detail. The pipeline
streams `dump | zstd | age | S3 multipart` with no intermediate disk, so the
format decisions also bound memory use and failure behaviour.

## Decision

**Dump format (PostgreSQL).** `pg_dump -Fc` (custom format) with `-Z0` —
built-in compression off, our zstd stage does the compressing. Custom format
buys selective restore of individual tables and parallel `pg_restore -j`; the
price is that restore goes through `pg_restore`, not plain `psql` (both steps
documented in `docs/restore.md`).

**Encryption is BYOK.** Backups are encrypted to the customer's `age`
recipient; dbferry only ever sees the public half. Losing the identity loses
the backups — documented honestly, no escrow.

**Object key schema (`key_schema: 1`, versioned in the manifest):**

```
<prefix>/<engine>/<cluster>/<database>/<YYYY>/<MM>/<backup_id>.dump.zst.age
<prefix>/<engine>/<cluster>/<database>/<YYYY>/<MM>/<backup_id>.manifest.json
```

`backup_id` = UTC timestamp + ULID, unique even for concurrent runs of the
same database. The date in the path exists for bucket lifecycle rules and
human navigation. Any change to this layout requires a new `key_schema`
version — never a silent change.

**Validity invariant: a backup is valid only with its manifest.** The manifest
is written only after `CompleteMultipartUpload` succeeds. A ciphertext object
without a manifest is an incomplete upload: subject to reconciliation/cleanup,
never reported as success, never counted by listing or retention. Only a
complete schema-1 manifest is treated as valid.

**Multipart upload defaults: 32 MiB parts, concurrency 4.** ~128 MiB of upload
buffers keeps total RSS within a 256 MiB budget with room for the Go runtime,
zstd and age; the maximum object is ~312 GiB (the 10k-part S3 limit). Hitting
10k parts raises a controlled error before `Complete`, suggesting a larger
part size in config — the part size is never changed automatically mid-upload
(predictable RSS). The values are configurable; the ones used are recorded in
each manifest.

## Consequences

- The manifest doubles as the integrity record (`ciphertext_sha256`, see
  ADR-0003) and the source of truth for listing and GFS retention
  (`ListBackups` / `SelectRetention` / `Prune` trust only manifest-complete
  backups).
- Restore tooling can rely on the key layout without talking to dbferry.
- Crash behaviour is self-healing by construction: an interrupted upload
  leaves ciphertext without a manifest (cleaned up via bucket lifecycle,
  `docs/operations.md`); prune deletes ciphertext before manifest, so a crash
  there leaves a dangling manifest that the next pass resolves.
