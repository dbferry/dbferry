package pipeline

import (
	"bytes"
	"context"
	"path"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// DestProbe reports which object-store operations a destination allows. Write is
// required for backups; Read is recommended; Delete is optional — an append-only
// bucket (write without delete) is a valid, safer policy, so a missing Delete is
// a warning, not a failure (poc-plan 5.3 review).
type DestProbe struct {
	Write, Read, Delete       bool
	WriteErr, ReadErr, DelErr error
	Leftover                  string // probe key left behind when Delete is not allowed
}

// ProbeDestination checks a destination by writing, reading and deleting a small
// probe object under a `.dbferry-probe/` prefix. cfg supplies the destination
// (Dest) and S3 auth (endpoint/region/profile/credentials).
func ProbeDestination(ctx context.Context, cfg Config) DestProbe {
	var p DestProbe
	client, err := newS3Client(ctx, cfg)
	if err != nil {
		p.WriteErr = err
		return p
	}
	dst, err := parseDest(cfg.Dest)
	if err != nil {
		p.WriteErr = err
		return p
	}
	key := path.Join(dst.prefix, ".dbferry-probe", "probe")

	if _, err := client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(dst.bucket), Key: aws.String(key),
		Body: bytes.NewReader([]byte("dbferry destination probe")),
	}); err != nil {
		p.WriteErr = err
		return p // no point probing read/delete without write
	}
	p.Write = true

	if _, err := client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(dst.bucket), Key: aws.String(key),
	}); err != nil {
		p.ReadErr = err
	} else {
		p.Read = true
	}

	if _, err := client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(dst.bucket), Key: aws.String(key),
	}); err != nil {
		p.DelErr = err
		p.Leftover = key
	} else {
		p.Delete = true
	}
	return p
}
