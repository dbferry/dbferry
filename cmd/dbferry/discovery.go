package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"

	"github.com/dbferry/dbferry/pipeline"
)

// connFlags are the flags shared by the read-only commands (test-connection,
// databases): a DSN source and output-mode switches.
type connFlags struct {
	dsnEnv, dsnFile *string
	jsonOut, quiet  *bool
	noColor         *bool
}

func registerConnFlags(fs *flag.FlagSet) connFlags {
	return connFlags{
		dsnEnv:  fs.String("dsn-env", "DBFERRY_DSN", "env var holding the DSN (never on argv)"),
		dsnFile: fs.String("dsn-file", "", "file holding the DSN (local dev; must be mode 0600)"),
		jsonOut: fs.Bool("json", false, "print a JSON result to stdout"),
		quiet:   fs.Bool("quiet", false, "suppress the success line; only errors"),
		noColor: fs.Bool("no-color", false, "disable ANSI colour"),
	}
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
	if err := fs.Parse(args); err != nil {
		return 1
	}
	out := cf.ui(stdout, stderr, stdoutTTY, stderrTTY)

	dsn, err := resolveDSN(*cf.dsnEnv, *cf.dsnFile)
	if err != nil {
		return out.fail(err, redactNothing)
	}
	redact := newRedactor(dsn)

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
	if err := fs.Parse(args); err != nil {
		return 1
	}
	out := cf.ui(stdout, stderr, stdoutTTY, stderrTTY)

	dsn, err := resolveDSN(*cf.dsnEnv, *cf.dsnFile)
	if err != nil {
		return out.fail(err, redactNothing)
	}
	redact := newRedactor(dsn)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	dbs, err := pipeline.ListDatabases(ctx, dsn)
	if err != nil {
		return out.fail(err, redact)
	}
	out.databases(dbs)
	return 0
}
