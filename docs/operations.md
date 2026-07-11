# Operating dbferry backups

## Cleaning up incomplete multipart uploads

dbferry streams each backup as an S3 multipart upload. On any failure — a dump
error, a network drop, Ctrl+C — dbferry **aborts** the multipart upload and
verifies the abort via `ListParts`, so no partial data is billed or left behind,
and no object is completed without its manifest.

That best-effort abort cannot run in every case: if the process is hard-killed
(`SIGKILL`, OOM, power loss) or S3 is unreachable at the moment of cleanup, an
incomplete multipart upload can survive. Because it is your bucket (BYOB), the
durable safety net is a **bucket lifecycle rule** that expires incomplete
multipart uploads. Configure it once, per backup bucket:

```json
{
  "Rules": [
    {
      "ID": "dbferry-abort-incomplete-multipart",
      "Status": "Enabled",
      "Filter": { "Prefix": "" },
      "AbortIncompleteMultipartUpload": { "DaysAfterInitiation": 7 }
    }
  ]
}
```

```sh
aws s3api put-bucket-lifecycle-configuration \
  --bucket my-backups \
  --lifecycle-configuration file://lifecycle.json
```

For S3-compatible storage add `--endpoint-url https://s3.example.com`. Support
for the `AbortIncompleteMultipartUpload` action varies by provider and version
(AWS S3 and Cloudflare R2 honour it; some MinIO builds silently drop it) — after
applying the rule, read it back with `aws s3api get-bucket-lifecycle-configuration`
to confirm your provider kept it.

`DaysAfterInitiation` is the grace period: keep it comfortably longer than your
longest backup run (a large database over a slow link can take hours). Seven days
is a safe default.

This rule and dbferry's verified abort are complementary: the abort reclaims
incomplete uploads immediately in the normal case; the lifecycle rule guarantees
eventual cleanup when the process couldn't.
