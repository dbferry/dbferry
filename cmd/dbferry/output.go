package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/dbferry/dbferry/pipeline"
)

// ui renders CLI output honouring --json, --quiet and --no-color, and keeps
// human progress on stderr so stdout carries only the machine-readable result.
type ui struct {
	stdout, stderr       io.Writer
	stdoutTTY, stderrTTY bool
	json, quiet, color   bool
}

func newFlagSet(name string, out io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(out)
	return fs
}

func usageErr(msg string) error { return errors.New(msg) }

// --- progress reporter ----------------------------------------------------

type reporter struct {
	w       io.Writer
	enabled bool
	lastLen int
}

// reporter renders live progress only for an interactive human: never with
// --json (stdout must stay a single JSON doc), --quiet, or a non-terminal
// stderr (logs/pipes shouldn't get carriage-return spam).
func (u *ui) reporter() *reporter {
	return &reporter{w: u.stderr, enabled: u.stderrTTY && !u.quiet && !u.json}
}

func (r *reporter) progress(p pipeline.Progress) {
	if !r.enabled {
		return
	}
	line := progressLine(p)
	pad := 0
	if r.lastLen > len(line) {
		pad = r.lastLen - len(line)
	}
	fmt.Fprintf(r.w, "\r%s%s", line, strings.Repeat(" ", pad))
	r.lastLen = len(line)
}

// finish clears the progress line so the summary/error that follows starts clean.
func (r *reporter) finish() {
	if r.enabled && r.lastLen > 0 {
		fmt.Fprintf(r.w, "\r%s\r", strings.Repeat(" ", r.lastLen))
	}
}

func progressLine(p pipeline.Progress) string {
	switch p.Phase {
	case pipeline.PhaseConnecting:
		return "  connecting…"
	case pipeline.PhaseFinalizing:
		return fmt.Sprintf("  finalizing… dumped %s · uploaded %s · %s",
			humanBytes(p.DumpedBytes), humanBytes(p.UploadedBytes), elapsed(p.Elapsed))
	case pipeline.PhaseDone:
		return fmt.Sprintf("  done · dumped %s · uploaded %s · %s",
			humanBytes(p.DumpedBytes), humanBytes(p.UploadedBytes), elapsed(p.Elapsed))
	default: // streaming
		return fmt.Sprintf("  backing up · dumped %s · uploaded %s · %s · %s",
			humanBytes(p.DumpedBytes), humanBytes(p.UploadedBytes),
			rate(p.UploadedBytes, p.Elapsed), elapsed(p.Elapsed))
	}
}

// --- result / error output ------------------------------------------------

type jsonResult struct {
	OK               bool   `json:"ok"`
	BackupID         string `json:"backup_id,omitempty"`
	Bucket           string `json:"bucket,omitempty"`
	Object           string `json:"object,omitempty"`
	Manifest         string `json:"manifest,omitempty"`
	CiphertextBytes  int64  `json:"ciphertext_bytes,omitempty"`
	CiphertextSHA256 string `json:"ciphertext_sha256,omitempty"`
	Kind             string `json:"kind,omitempty"`
	Error            string `json:"error,omitempty"`
}

func (u *ui) success(res pipeline.Result) {
	if u.json {
		u.writeJSON(jsonResult{
			OK: true, BackupID: res.BackupID, Bucket: res.Bucket, Object: res.Key,
			Manifest: res.ManifestKey, CiphertextBytes: res.Bytes, CiphertextSHA256: res.SHA256,
		})
		return
	}
	if u.quiet {
		return
	}
	fmt.Fprintf(u.stdout, "%s\n  backup_id  %s\n  object     s3://%s/%s\n  manifest   s3://%s/%s\n  uploaded   %s (ciphertext)\n  sha256     %s\n",
		paint(u.color && u.stdoutTTY, colorGreen, "backup complete"),
		res.BackupID, res.Bucket, res.Key, res.Bucket, res.ManifestKey, humanBytes(res.Bytes), res.SHA256)
}

// fail prints a classified, redacted error with an actionable next step and
// returns the process exit code.
func (u *ui) fail(err error, redact func(string) string) int {
	kind := pipeline.KindOf(err)
	msg := redact(err.Error())
	if u.json {
		u.writeJSON(jsonResult{OK: false, Kind: kind.String(), Error: msg})
		return exitCode(err)
	}
	fmt.Fprintln(u.stderr, paint(u.color && u.stderrTTY, colorRed, "dbferry: ")+msg)
	if h := hint(kind, msg); h != "" {
		fmt.Fprintln(u.stderr)
		fmt.Fprintln(u.stderr, "  → "+redact(h))
	}
	return exitCode(err)
}

// warn prints a non-fatal warning to stderr (kept off stdout so --json stays a
// single clean document). Shown regardless of --quiet: a safety caveat matters.
func (u *ui) warn(msg string) {
	fmt.Fprintln(u.stderr, paint(u.color && u.stderrTTY, colorYellow, "warning: ")+msg)
}

func (u *ui) writeJSON(r jsonResult) { u.writeJSONValue(r) }

func (u *ui) writeJSONValue(v any) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		fmt.Fprintf(u.stderr, "dbferry: encode json: %v\n", err)
		return
	}
	fmt.Fprintln(u.stdout, string(b))
}

// databases renders the discovery result: names, with inaccessible ones flagged
// (not hidden) so a missing grant is visible.
func (u *ui) databases(dbs []pipeline.DatabaseInfo) {
	if u.json {
		u.writeJSONValue(struct {
			OK        bool                    `json:"ok"`
			Databases []pipeline.DatabaseInfo `json:"databases"`
		}{OK: true, Databases: dbs})
		return
	}
	if len(dbs) == 0 {
		fmt.Fprintln(u.stdout, "no user databases found")
		return
	}
	for _, d := range dbs {
		if d.Accessible {
			fmt.Fprintln(u.stdout, "  "+d.Name)
		} else {
			fmt.Fprintln(u.stdout, "  "+d.Name+paint(u.color && u.stdoutTTY, colorYellow, "  [no access — grant CONNECT]"))
		}
	}
}

// hint maps a failure to a concrete next step: the grant, policy or command to
// run (poc-plan 3.3). It reads only the already-redacted message.
func hint(kind pipeline.Kind, msg string) string {
	switch kind {
	case pipeline.KindConnect:
		return "Check the host, port, database name and credentials, and that the role may CONNECT. Fix them and re-run the same command."
	case pipeline.KindDump:
		switch {
		case strings.Contains(msg, "executable file not found"), strings.Contains(msg, "not found in $PATH"):
			if strings.Contains(msg, "mysqldump") {
				return "mysqldump is not on PATH. Install the MySQL client tools (e.g. mysql-client / mysql-community-client), then re-run."
			}
			return "pg_dump is not on PATH. Install the PostgreSQL client tools (e.g. postgresql-client) at your server's major version or newer, then re-run."
		case strings.Contains(msg, "non-InnoDB"):
			return "Some tables use a non-transactional engine (e.g. MyISAM). Re-run with --allow-nontransactional to back them up (their snapshot may be inconsistent), or convert them to InnoDB."
		case strings.Contains(msg, "permission denied"):
			return "The role can connect but can't read some objects. Grant read access (e.g. GRANT pg_read_all_data TO <role>) or run as a role with SELECT on all objects, then re-run."
		default:
			return "The dump failed; see its message above. Fix the reported object or permission and re-run."
		}
	case pipeline.KindUpload:
		return "Check the bucket exists and your S3 credentials allow s3:CreateMultipartUpload, UploadPart and PutObject on it (and that --s3-endpoint is correct for S3-compatible storage)."
	case pipeline.KindCanceled:
		return "The run was canceled and no backup was written. Re-run when ready."
	}
	return ""
}

// --- redaction ------------------------------------------------------------

func redactNothing(s string) string { return s }

// newRedactor scrubs the DSN and its password from any string before it reaches
// stdout, stderr or logs (poc-plan 3.3).
func newRedactor(dsn string) func(string) string {
	var secrets []string
	if dsn != "" {
		secrets = append(secrets, dsn)
	}
	if pw := passwordOf(dsn); pw != "" {
		secrets = append(secrets, pw)
	}
	if len(secrets) == 0 {
		return redactNothing
	}
	return func(s string) string {
		for _, sec := range secrets {
			s = strings.ReplaceAll(s, sec, "[redacted]")
		}
		return s
	}
}

func passwordOf(dsn string) string {
	u, err := url.Parse(dsn)
	if err != nil || u.User == nil {
		return ""
	}
	pw, _ := u.User.Password()
	return pw
}

// --- formatting helpers ---------------------------------------------------

const (
	colorRed    = "\x1b[31m"
	colorGreen  = "\x1b[32m"
	colorYellow = "\x1b[33m"
	colorReset  = "\x1b[0m"
)

func paint(enabled bool, code, s string) string {
	if !enabled {
		return s
	}
	return code + s + colorReset
}

// parsePartSize parses a multipart part size like "32MiB", "64MB" or a bare
// number (interpreted as MiB), enforcing the 5 MiB S3 minimum for parts.
func parsePartSize(s string) (int64, error) {
	s = strings.TrimSpace(s)
	mult := int64(1) << 20 // bare number → MiB
	switch l := strings.ToLower(s); {
	case strings.HasSuffix(l, "gib"):
		mult, s = 1<<30, s[:len(s)-3]
	case strings.HasSuffix(l, "mib"):
		mult, s = 1<<20, s[:len(s)-3]
	case strings.HasSuffix(l, "gb"):
		mult, s = 1_000_000_000, s[:len(s)-2]
	case strings.HasSuffix(l, "mb"):
		mult, s = 1_000_000, s[:len(s)-2]
	}
	n, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("invalid --part-size %q (e.g. 32MiB)", s)
	}
	b := int64(n * float64(mult))
	if b < 5<<20 {
		return 0, fmt.Errorf("--part-size must be at least 5MiB (the S3 minimum for a multipart part)")
	}
	return b, nil
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for x := n / unit; x >= unit; x /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

func rate(bytes int64, d time.Duration) string {
	if d <= 0 || bytes <= 0 {
		return "0 B/s"
	}
	return humanBytes(int64(float64(bytes)/d.Seconds())) + "/s"
}

func elapsed(d time.Duration) string {
	return d.Truncate(100 * time.Millisecond).String()
}
