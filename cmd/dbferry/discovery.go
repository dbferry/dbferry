package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"

	"github.com/dbferry/dbferry/config"
	"github.com/dbferry/dbferry/pipeline"
)

// connFlags are the flags shared by the read-only commands (test-connection,
// databases): a DSN source (a named connection or --dsn-*) and output switches.
type connFlags struct {
	connName, cfgPath *string
	dsnEnv, dsnFile   *string
	jsonOut, quiet    *bool
	noColor           *bool
}

func registerConnFlags(fs *flag.FlagSet) connFlags {
	return connFlags{
		connName: fs.String("connection", "", "named connection from the config (instead of --dsn-*)"),
		cfgPath:  fs.String("config", "", "config file path (with --connection)"),
		dsnEnv:   fs.String("dsn-env", "DBFERRY_DSN", "env var holding the DSN (never on argv)"),
		dsnFile:  fs.String("dsn-file", "", "file holding the DSN (local dev; must be mode 0600)"),
		jsonOut:  fs.Bool("json", false, "print a JSON result to stdout"),
		quiet:    fs.Bool("quiet", false, "suppress the success line; only errors"),
		noColor:  fs.Bool("no-color", false, "disable ANSI colour"),
	}
}

// conflict reports a contradictory flag combination: a named connection plus
// explicit --dsn-* flags would silently ignore one of the two sources.
func (c connFlags) conflict(fs *flag.FlagSet) error {
	if *c.connName != "" && (*c.dsnFile != "" || flagWasSet(fs, "dsn-env")) {
		return usageErr("choose either --connection or --dsn-env/--dsn-file, not both")
	}
	return nil
}

// source resolves the DSN to connect with (a named connection's connect DSN, or
// --dsn-*) plus a redactor covering its secrets.
func (c connFlags) source() (dsn string, redact func(string) string, err error) {
	if *c.connName != "" {
		cfg, lerr := config.Load(configPath(*c.cfgPath))
		if lerr != nil {
			return "", redactNothing, lerr
		}
		conn := cfg.Connections[*c.connName]
		if conn == nil {
			return "", redactNothing, fmt.Errorf("no connection named %q (see `dbferry connections list`)", *c.connName)
		}
		d, secrets, cerr := conn.ConnectDSN()
		if cerr != nil {
			return "", redactNothing, cerr
		}
		var red config.Redactor
		red.Add(secrets...)
		return d, red.Redact, nil
	}
	d, derr := resolveDSN(*c.dsnEnv, *c.dsnFile)
	if derr != nil {
		return "", redactNothing, derr
	}
	var red config.Redactor
	red.Add(d)
	if pw := passwordOf(d); pw != "" {
		red.Add(pw)
	}
	return d, red.Redact, nil
}

func (c connFlags) ui(stdout, stderr io.Writer, stdoutTTY, stderrTTY bool) *ui {
	return &ui{
		stdout: stdout, stderr: stderr,
		stdoutTTY: stdoutTTY, stderrTTY: stderrTTY,
		json: *c.jsonOut, quiet: *c.quiet, color: !*c.noColor,
	}
}

// cmdTestConnection verifies the database is reachable and authenticated
// without running a backup (poc-plan 5.1).
func cmdTestConnection(args []string, stdout, stderr io.Writer, stdoutTTY, stderrTTY bool) int {
	fs := newFlagSet("test-connection", stderr)
	cf := registerConnFlags(fs)
	if code, ok := parseFlags(fs, args); !ok {
		return code
	}
	out := cf.ui(stdout, stderr, stdoutTTY, stderrTTY)
	if err := cf.conflict(fs); err != nil {
		return out.fail(err, redactNothing)
	}
	dsn, redact, err := cf.source()
	if err != nil {
		return out.fail(err, redactNothing)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	if err := pipeline.TestConnection(ctx, dsn); err != nil {
		return out.fail(err, redact)
	}
	if out.json {
		out.writeJSON(jsonResult{OK: true})
	} else if !out.quiet {
		fmt.Fprintln(out.stdout, paint(out.color && out.stdoutTTY, colorGreen, "connection ok"))
	}
	return 0
}

// cmdDatabases lists the user databases in the cluster the DSN points at, each
// flagged accessible or not (poc-plan 5.2).
func cmdDatabases(args []string, stdout, stderr io.Writer, stdoutTTY, stderrTTY bool) int {
	fs := newFlagSet("databases", stderr)
	cf := registerConnFlags(fs)
	if code, ok := parseFlags(fs, args); !ok {
		return code
	}
	out := cf.ui(stdout, stderr, stdoutTTY, stderrTTY)
	if err := cf.conflict(fs); err != nil {
		return out.fail(err, redactNothing)
	}
	dsn, redact, err := cf.source()
	if err != nil {
		return out.fail(err, redactNothing)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	dbs, err := pipeline.ListDatabases(ctx, dsn)
	if err != nil {
		return out.fail(err, redact)
	}
	out.databases(dbs)
	return 0
}
