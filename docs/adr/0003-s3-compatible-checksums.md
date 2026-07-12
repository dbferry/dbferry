# ADR-0003: Disable default SDK request checksums for S3-compatible endpoints

Date: 2026-07-12
Status: accepted

## Context

dbferry is BYOB: customers back up to their own object storage, which is often
an S3-compatible provider rather than AWS S3 itself — DigitalOcean Spaces,
Cloudflare R2, Backblaze B2, MinIO. The DigitalOcean audience is a primary
target (managed Postgres customers who already run on DO).

`aws-sdk-go-v2` (≥ ~2025 releases) enables data-integrity request checksums by
default: `RequestChecksumCalculation = WhenSupported` makes it attach a CRC32
checksum (as an `x-amz-checksum-crc32` header or a streaming trailer) to
`PutObject`/`UploadPart`. AWS S3 accepts these; several S3-compatible providers
do **not** — DigitalOcean Spaces rejects the request, and some MinIO/R2 builds
behave inconsistently. This surfaced on the first real end-to-end run: backing
up a real DO Managed PostgreSQL database to a real DO Space failed at upload
until the checksums were disabled.

The uploaded object is still integrity-protected regardless: every request is
SigV4-signed over the payload SHA-256, so a corrupted or tampered body is
rejected at signature verification. The CRC32 layer is redundant belt-and-braces,
not the sole guarantee. dbferry also records its own SHA-256 of the ciphertext in
the manifest for independent verification.

## Decision

When a custom endpoint is configured (`--s3-endpoint` set, i.e. S3-compatible
mode), build the S3 client with:

- `RequestChecksumCalculation = aws.RequestChecksumCalculationWhenRequired`
- `ResponseChecksumValidation = aws.ResponseChecksumValidationWhenRequired`

For real AWS S3 (no custom endpoint) the SDK defaults are left unchanged, keeping
the extra integrity checks where the provider supports them. The switch lives in
`pipeline`'s S3 client construction and is gated purely on endpoint presence.

## Consequences

- DigitalOcean Spaces, Cloudflare R2, Backblaze B2 and MinIO work out of the box;
  this would otherwise have blocked *every* customer on S3-compatible storage.
- No loss of integrity: SigV4 signed-SHA256 still protects each request, and the
  manifest's `ciphertext_sha256` gives an independent end-to-end check.
- The behaviour differs by endpoint (AWS vs compatible). Acceptable and
  intentional; documented here so it isn't "fixed" back to a single code path.
- Not unit-testable in isolation (MinIO accepts both modes; provider-specific
  rejection can't be reproduced locally) — validated by a real DO Spaces run.
  If a future S3-compatible provider needs checksums, revisit with a new ADR.
