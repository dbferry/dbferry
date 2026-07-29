// Command dbferry is the standalone CLI that drives the backup pipeline.
// The same pipeline package is later invoked from River jobs unchanged
// (ADR-0001); this binary keeps it usable on its own.
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"

	"github.com/dbferry/dbferry/config"
	"github.com/dbferry/dbferry/pipeline"
)

// version is overridden at build time via -ldflags "-X main.version=...".
var version = "0.0.0-dev"

func main() {
	code := run(os.Args[1:], os.Stdout, os.Stderr, isTTY(os.Stdout), isTTY(os.Stderr))
	os.Exit(code)
}

// isTTY reports whether f is an interactive terminal (not a pipe or file), so
// progress and colour are only used where a human is watching.
func isTTY(f *os.File) bool {
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

// exitCode maps a failure to a distinct code so automation can tell a
// connection problem from a dump problem from an upload problem (poc-plan 2.5).
func exitCode(err error) int {
	switch pipeline.KindOf(err) {
	case pipeline.KindConnect:
		return 3
	case pipeline.KindDump:
		return 4
	case pipeline.KindUpload:
		return 5
	case pipeline.KindCanceled:
		return 130
	default:
		return 1
	}
}

func run(args []string, stdout, stderr io.Writer, stdoutTTY, stderrTTY bool) int {
	if len(args) == 0 {
		usage(stderr)
		fmt.Fprintln(stderr, "dbferry: no command given")
		return 1
	}
	switch args[0] {
	case "init":
		return cmdInit(args[1:], stdout, stderr, stdoutTTY, stderrTTY)
	case "keygen":
		return cmdKeygen(args[1:], stdout, stderr)
	case "run":
		return cmdRun(args[1:], stdout, stderr, stdoutTTY, stderrTTY)
	case "test-connection":
		return cmdTestConnection(args[1:], stdout, stderr, stdoutTTY, stderrTTY)
	case "databases":
		return cmdDatabases(args[1:], stdout, stderr, stdoutTTY, stderrTTY)
	case "connections":
		return cmdConnections(args[1:], stdout, stderr)
	case "destinations":
		return cmdDestinations(args[1:], stdout, stderr)
	case "doctor":
		return cmdDoctor(args[1:], stdout, stderr, stdoutTTY, stderrTTY)
	case "version", "--version", "-v":
		if len(args) > 1 {
			fmt.Fprintf(stderr, "dbferry version: unexpected argument %q\n", args[1])
			return 1
		}
		fmt.Fprintln(stdout, version)
		return 0
	case "help", "-h", "--help":
		usage(stdout)
		return 0
	default:
		usage(stderr)
		fmt.Fprintf(stderr, "dbferry: unknown command %q\n", args[0])
		return 1
	}
}

func cmdRun(args []string, stdout, stderr io.Writer, stdoutTTY, stderrTTY bool) int {
	fs := newFlagSet("run", stderr)
	var (
		connName     = fs.String("connection", "", "named connection from the config (instead of --dsn-*/--dest)")
		database     = fs.String("database", "", "database to back up (with --connection; overrides default_database)")
		destName     = fs.String("destination", "", "named destination (with --connection; overrides the connection's default)")
		cfgPath      = fs.String("config", "", "config file path (with --connection)")
		dsnEnv       = fs.String("dsn-env", "DBFERRY_DSN", "name of the env var holding the database DSN (the DSN is never passed on argv)")
		dsnFile      = fs.String("dsn-file", "", "path to a file holding the DSN; local dev only, must be mode 0600")
		dest         = fs.String("dest", "", "destination, e.g. s3://bucket/prefix")
		ageRecipient = fs.String("age-recipient", "", "age public recipient to encrypt the backup to")
		s3Endpoint   = fs.String("s3-endpoint", "", "S3-compatible endpoint URL (e.g. http://localhost:9000 for MinIO); empty for AWS S3")
		partSize     = fs.String("part-size", "", "multipart part size, e.g. 32MiB (min 5MiB, the S3 minimum); default 32MiB")
		concurrency  = fs.Int("concurrency", 0, "number of parts uploaded in parallel; default 4")
		allowNonTx   = fs.Bool("allow-nontransactional", false, "allow backing up non-transactional tables (e.g. MySQL MyISAM) that can't be snapshotted consistently")
		jsonOut      = fs.Bool("json", false, "print one JSON result object to stdout instead of human output")
		quiet        = fs.Bool("quiet", false, "suppress progress and the success summary; only errors are printed")
		noColor      = fs.Bool("no-color", false, "disable ANSI colour")
	)
	if code, ok := parseFlags(fs, args); !ok {
		return code
	}

	out := &ui{
		stdout: stdout, stderr: stderr,
		stdoutTTY: stdoutTTY, stderrTTY: stderrTTY,
		json: *jsonOut, quiet: *quiet, color: !*noColor,
	}

	// Resolve source + destination either from a named connection or from the
	// standalone flags. A single Redactor collects every secret touched (DB
	// password, S3 keys) so it is scrubbed from all output.
	var (
		red                       config.Redactor
		dsn, destURL, recipient   string
		endpoint, region, profile string
		s3creds                   *pipeline.S3Credentials
		err                       error
	)

	if *connName != "" {
		if *dsnFile != "" || flagWasSet(fs, "dsn-env") {
			return out.fail(usageErr("choose either --connection or --dsn-env/--dsn-file, not both"), redactNothing)
		}
		cfg, lerr := config.Load(configPath(*cfgPath))
		if lerr != nil {
			return out.fail(lerr, redactNothing)
		}
		conn := cfg.Connections[*connName]
		if conn == nil {
			return out.fail(usageErr(fmt.Sprintf("no connection named %q (see `dbferry connections list`)", *connName)), redactNothing)
		}
		var secrets []string
		dsn, secrets, err = conn.BackupDSN(*database)
		if err != nil {
			return out.fail(err, red.Redact)
		}
		red.Add(secrets...)
		recipient = conn.AgeRecipient

		dn := *destName
		if dn == "" {
			dn = conn.Destination
		}
		if dn != "" {
			dst := cfg.Destinations[dn]
			if dst == nil {
				return out.fail(usageErr(fmt.Sprintf("no destination named %q", dn)), red.Redact)
			}
			s3, dsecrets, rerr := dst.Resolve()
			if rerr != nil {
				return out.fail(rerr, red.Redact)
			}
			red.Add(dsecrets...)
			destURL, endpoint, region, profile = dst.DestURL(), s3.Endpoint, s3.Region, s3.Profile
			if s3.HasStatic {
				s3creds = &pipeline.S3Credentials{AccessKeyID: s3.AccessKey, SecretAccessKey: s3.SecretKey, SessionToken: s3.SessionToken}
			}
		}
	} else {
		dsn, err = resolveDSN(*dsnEnv, *dsnFile)
		if err != nil {
			return out.fail(err, redactNothing)
		}
		red.Add(dsn)
		if pw := passwordOf(dsn); pw != "" {
			red.Add(pw)
		}
	}

	// Explicit flags override connection defaults.
	if *dest != "" {
		destURL = *dest
	}
	if *ageRecipient != "" {
		recipient = *ageRecipient
	}
	if *s3Endpoint != "" {
		endpoint = *s3Endpoint
	}
	redact := red.Redact

	if destURL == "" {
		return out.fail(usageErr("no destination: pass --dest or set one on the connection"), redact)
	}
	if recipient == "" {
		return out.fail(usageErr("no age recipient: pass --age-recipient or set age_recipient on the connection"), redact)
	}
	var ps int64
	if *partSize != "" {
		ps, err = parsePartSize(*partSize)
		if err != nil {
			return out.fail(usageErr(err.Error()), redact)
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	rep := out.reporter()
	res, err := pipeline.Run(ctx, pipeline.Config{
		DSN:                   dsn,
		Dest:                  destURL,
		AgeRecipient:          recipient,
		S3Endpoint:            endpoint,
		S3Region:              region,
		S3Profile:             profile,
		S3Credentials:         s3creds,
		PartSize:              ps,
		Concurrency:           *concurrency,
		AppVersion:            version,
		AllowNonTransactional: *allowNonTx,
		Warn:                  out.warn,
		Progress:              rep.progress,
	})
	rep.finish()

	if err != nil {
		return out.fail(err, redact)
	}
	out.success(res)
	return 0
}

// resolveDSN loads the DSN from a file (local dev) or an env var, so the secret
// never appears on the command line. A file must not be readable by group or
// other.
func resolveDSN(envName, file string) (string, error) {
	if file != "" {
		info, err := os.Stat(file)
		if err != nil {
			return "", fmt.Errorf("--dsn-file: %w", err)
		}
		if err := config.RequirePrivate("--dsn-file", file, info); err != nil {
			return "", err
		}
		b, err := os.ReadFile(file)
		if err != nil {
			return "", fmt.Errorf("--dsn-file: %w", err)
		}
		dsn := strings.TrimSpace(string(b))
		if dsn == "" {
			return "", fmt.Errorf("--dsn-file %s is empty", file)
		}
		return dsn, nil
	}

	dsn := os.Getenv(envName)
	if dsn == "" {
		return "", fmt.Errorf("no DSN found: set $%s or pass --dsn-file", envName)
	}
	return dsn, nil
}

func usage(w io.Writer) {
	fmt.Fprint(w, `dbferry — per-database backups for managed PostgreSQL/MySQL

usage:
  dbferry init                                     interactive setup of a named connection
  dbferry keygen --out PATH                        generate an age identity
  dbferry run --connection NAME [--database DB]    back up using a named connection
  dbferry run --dest s3://bucket/prefix --age-recipient age1... [--dsn-env DBFERRY_DSN | --dsn-file PATH]
  dbferry test-connection [--connection NAME | --dsn-env DBFERRY_DSN | --dsn-file PATH]
  dbferry databases [--dsn-env DBFERRY_DSN | --dsn-file PATH] [--json]
  dbferry doctor [--connection NAME]               diagnose source + destination
  dbferry connections  <list|show|add|rm> ...
  dbferry destinations <list|show|add|rm> ...
  dbferry version
  dbferry help

the DSN scheme selects the engine: postgres:// (or postgresql://) or mysql://

run flags:
  --dsn-env NAME     env var holding the DSN (default DBFERRY_DSN); never on argv
  --dsn-file PATH    file holding the DSN (local dev; must be mode 0600)
  --dest URL         destination, e.g. s3://bucket/prefix
  --age-recipient R  age public recipient to encrypt to (BYOK)
  --s3-endpoint URL  S3-compatible endpoint (e.g. MinIO); empty for AWS S3
  --part-size SIZE   multipart part size, e.g. 32MiB (min 5MiB); default 32MiB
  --concurrency N    parts uploaded in parallel; default 4
  --allow-nontransactional  allow non-InnoDB (e.g. MyISAM) MySQL tables
  --json             one JSON result object on stdout (for scripts)
  --quiet            no progress or summary (for cron)
  --no-color         disable ANSI colour (for logs)

exit codes:
  0 success   3 connect error   4 dump error   5 upload error   130 canceled
`)
}
