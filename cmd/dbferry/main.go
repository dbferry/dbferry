// Command dbferry is the standalone CLI that drives the backup pipeline.
// The same pipeline package is later invoked from River jobs unchanged
// (ADR-0001); this binary keeps it usable on its own.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"

	"github.com/dbferry/dbferry/pipeline"
)

// version is overridden at build time via -ldflags "-X main.version=...".
var version = "0.0.0-dev"

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "dbferry: "+err.Error())
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		usage(os.Stderr)
		return errors.New("no command given")
	}
	switch args[0] {
	case "run":
		return cmdRun(args[1:])
	case "version", "--version", "-v":
		fmt.Println(version)
		return nil
	case "help", "-h", "--help":
		usage(os.Stdout)
		return nil
	default:
		usage(os.Stderr)
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func cmdRun(args []string) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	var (
		dsnEnv       = fs.String("dsn-env", "DBFERRY_DSN", "name of the env var holding the database DSN (the DSN is never passed on argv)")
		dsnFile      = fs.String("dsn-file", "", "path to a file holding the DSN; local dev only, must be mode 0600")
		dest         = fs.String("dest", "", "destination, e.g. s3://bucket/prefix")
		ageRecipient = fs.String("age-recipient", "", "age public recipient to encrypt the backup to")
	)
	if err := fs.Parse(args); err != nil {
		return err
	}

	dsn, err := resolveDSN(*dsnEnv, *dsnFile)
	if err != nil {
		return err
	}
	if *dest == "" {
		return errors.New("--dest is required (e.g. s3://bucket/prefix)")
	}
	if *ageRecipient == "" {
		return errors.New("--age-recipient is required (BYOK: your age public key)")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	return pipeline.Run(ctx, pipeline.Config{
		DSN:          dsn,
		Dest:         *dest,
		AgeRecipient: *ageRecipient,
	})
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
		if perm := info.Mode().Perm(); perm&0o077 != 0 {
			return "", fmt.Errorf("--dsn-file %s has insecure permissions %#o; want 0600", file, perm)
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

func usage(w *os.File) {
	fmt.Fprint(w, `dbferry — per-database backups for managed PostgreSQL/MySQL

usage:
  dbferry run --dest s3://bucket/prefix --age-recipient age1... [--dsn-env DBFERRY_DSN | --dsn-file PATH]
  dbferry version
  dbferry help
`)
}
