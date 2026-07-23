package main

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"

	"filippo.io/age"

	"github.com/dbferry/dbferry/config"
	"github.com/dbferry/dbferry/pipeline"
)

// cmdKeygen generates an age identity to an explicit path (poc-plan 5.3/0.5.3):
// never hidden inside init, never overwriting, always 0600.
func cmdKeygen(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("keygen", stderr)
	out := fs.String("out", "", "path to write the age identity (required)")
	if err := fs.Parse(args); err != nil {
		return 1
	}
	if *out == "" {
		fmt.Fprintln(stderr, "usage: dbferry keygen --out PATH")
		return 1
	}
	// O_EXCL: refuse to overwrite an existing identity.
	f, err := os.OpenFile(*out, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		fmt.Fprintf(stderr, "dbferry: %v (refusing to overwrite)\n", err)
		return 1
	}
	id, err := age.GenerateX25519Identity()
	if err != nil {
		f.Close()
		os.Remove(*out)
		fmt.Fprintf(stderr, "dbferry: generate identity: %v\n", err)
		return 1
	}
	if _, err := fmt.Fprintln(f, id.String()); err != nil {
		f.Close()
		return 1
	}
	f.Close()
	fmt.Fprintf(stdout, "wrote identity to %s (0600)\nrecipient: %s\n\n"+
		"KEEP THIS FILE SAFE. It is the only key that can decrypt your backups —\n"+
		"dbferry never holds it. Store a copy in a password manager and offline.\n",
		*out, id.Recipient())
	return 0
}

// cmdInit is the interactive setup wizard: one connection, its destination and
// recipient, verified end to end, then saved (poc-plan 0.5.3).
func cmdInit(args []string, stdout, stderr io.Writer, stdoutTTY, stderrTTY bool) int {
	fs := newFlagSet("init", stderr)
	cfgPath := fs.String("config", "", "config file path")
	if err := fs.Parse(args); err != nil {
		return 1
	}
	path := configPath(*cfgPath)

	fmt.Fprint(stderr, "dbferry init — set up a named connection\n\n")

	name := promptLine("connection name: ")
	if name == "" {
		fmt.Fprintln(stderr, "dbferry: a name is required")
		return 1
	}
	engine, template, password, err := splitDSN(promptLine("database DSN (postgres://… or mysql://…): "))
	if err != nil {
		fmt.Fprintln(stderr, "dbferry: "+err.Error())
		return 1
	}
	if password == "" {
		password = readSecret(stderr, "database password: ")
	}
	if password == "" {
		fmt.Fprintln(stderr, "dbferry: a password is required to verify the connection")
		return 1
	}

	// Where the password is stored.
	var pwRef config.SecretRef
	if strings.HasPrefix(strings.ToLower(promptLine("store password in [K]eychain or [e]nv-ref? ")), "e") {
		envVar := promptLine("env var name (e.g. DBFERRY_" + strings.ToUpper(name) + "_PASS): ")
		if envVar == "" {
			fmt.Fprintln(stderr, "dbferry: env var name required")
			return 1
		}
		pwRef = config.SecretRef{Env: envVar}
	} else {
		pwRef = config.SecretRef{Keyring: "dbferry/" + name}
	}
	defaultDB := promptLine("default database to back up (optional): ")

	// Verify the connection with the password we have in memory.
	testDSN, err := injectPw(template, password)
	if err != nil {
		fmt.Fprintln(stderr, "dbferry: "+err.Error())
		return 1
	}
	ctx := context.Background()
	if err := pipeline.TestConnection(ctx, testDSN); err != nil {
		fmt.Fprintf(stderr, "dbferry: connection failed: %v\n", err)
		return 1
	}
	fmt.Fprintln(stderr, "  ✓ connection ok")
	if dbs, err := pipeline.ListDatabases(ctx, testDSN); err == nil && len(dbs) > 0 {
		fmt.Fprintln(stderr, "  databases:")
		for _, d := range dbs {
			mark := ""
			if !d.Accessible {
				mark = "  [no access]"
			}
			fmt.Fprintf(stderr, "    %s%s\n", d.Name, mark)
		}
	}

	// Destination: reuse an existing one or create a new one.
	destName, err := chooseDestination(path, stderr)
	if err != nil {
		fmt.Fprintln(stderr, "dbferry: "+err.Error())
		return 1
	}

	// age recipient.
	rcpt := promptLine("age recipient (age1…; create one with `dbferry keygen`): ")
	if _, err := age.ParseX25519Recipient(rcpt); err != nil {
		fmt.Fprintf(stderr, "dbferry: invalid age recipient: %v\n", err)
		return 1
	}

	conn := &config.Connection{
		Engine: engine, DSN: template, Password: pwRef, DefaultDatabase: defaultDB,
		Destination: destName, AgeRecipient: rcpt,
	}
	if err := conn.Validate(); err != nil {
		fmt.Fprintf(stderr, "dbferry: %v\n", err)
		return 1
	}

	// Store keychain secret first, then config; roll back on failure.
	if pwRef.Keyring != "" {
		if err := pwRef.Store(password); err != nil {
			fmt.Fprintln(stderr, "dbferry: "+err.Error())
			return 1
		}
	}
	unlock, err := config.Lock(path)
	if err != nil {
		_ = pwRef.Delete()
		fmt.Fprintln(stderr, "dbferry: "+err.Error())
		return 1
	}
	defer unlock()
	cfg, err := config.Load(path)
	if err != nil {
		_ = pwRef.Delete()
		fmt.Fprintln(stderr, "dbferry: "+err.Error())
		return 1
	}
	cfg.Connections[name] = conn
	if err := cfg.SaveLocked(path); err != nil {
		_ = pwRef.Delete()
		fmt.Fprintln(stderr, "dbferry: "+err.Error())
		return 1
	}

	fmt.Fprintf(stdout, "\nsaved connection %q → %s\nback it up with:\n  dbferry run --connection %s%s\n",
		name, path, name, databaseHint(defaultDB))
	if pwRef.Env != "" {
		fmt.Fprintf(stdout, "\nremember to set the password env var: export %s=…\n", pwRef.Env)
	}
	return 0
}

// chooseDestination lets the user pick an existing destination or create one,
// then probes it (write/read/delete).
func chooseDestination(cfgPath string, stderr io.Writer) (string, error) {
	// Hold the config lock across the whole choose-or-create (interactive, so
	// released only when this returns); the surrounding init save re-locks
	// afterwards, sequentially.
	unlock, err := config.Lock(cfgPath)
	if err != nil {
		return "", err
	}
	defer unlock()
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return "", err
	}
	if names := cfg.DestinationNames(); len(names) > 0 {
		fmt.Fprintf(stderr, "existing destinations: %s\n", strings.Join(names, ", "))
	}
	name := promptLine("destination name (existing, or new to create): ")
	if name == "" {
		return "", fmt.Errorf("a destination is required")
	}
	if _, ok := cfg.Destinations[name]; ok {
		return name, nil
	}

	// Create a new destination.
	dst := &config.Destination{
		Bucket:   promptLine("  bucket / Space: "),
		Prefix:   promptLine("  prefix (optional): "),
		Endpoint: promptLine("  endpoint (empty for AWS S3): "),
		Region:   promptLine("  region: "),
	}
	if ak := promptLine("  access key env var (empty → AWS default chain): "); ak != "" {
		dst.AccessKey = &config.SecretRef{Env: ak}
		sk := promptLine("  secret key env var: ")
		dst.SecretKey = &config.SecretRef{Env: sk}
	}
	if err := dst.Validate(); err != nil {
		return "", err
	}
	cfg.Destinations[name] = dst
	if err := cfg.SaveLocked(cfgPath); err != nil {
		return "", err
	}

	// Probe it.
	s3, _, err := dst.Resolve()
	if err != nil {
		fmt.Fprintf(stderr, "  (skipping destination test: %v)\n", err)
		return name, nil
	}
	p := pipeline.ProbeDestination(context.Background(), destProbeConfig(dst, s3))
	reportProbe(stderr, p)
	return name, nil
}

func destProbeConfig(dst *config.Destination, s3 config.S3Settings) pipeline.Config {
	cfg := pipeline.Config{
		Dest: dst.DestURL(), S3Endpoint: s3.Endpoint, S3Region: s3.Region, S3Profile: s3.Profile,
	}
	if s3.HasStatic {
		cfg.S3Credentials = &pipeline.S3Credentials{
			AccessKeyID: s3.AccessKey, SecretAccessKey: s3.SecretKey, SessionToken: s3.SessionToken,
		}
	}
	return cfg
}

func reportProbe(stderr io.Writer, p pipeline.DestProbe) {
	if !p.Write {
		fmt.Fprintf(stderr, "  ✗ write FAILED: %v (backups need write access)\n", p.WriteErr)
		return
	}
	fmt.Fprintln(stderr, "  ✓ write ok")
	if p.Read {
		fmt.Fprintln(stderr, "  ✓ read ok")
	} else {
		fmt.Fprintf(stderr, "  ! read not available: %v\n", p.ReadErr)
	}
	if p.Delete {
		fmt.Fprintln(stderr, "  ✓ delete ok")
	} else {
		fmt.Fprintf(stderr, "  ! delete not available (append-only bucket is fine): %v\n  probe object left at %s — remove it manually\n", p.DelErr, p.Leftover)
	}
}

func injectPw(template, password string) (string, error) {
	u, err := url.Parse(template)
	if err != nil {
		return "", fmt.Errorf("invalid DSN")
	}
	u.User = url.UserPassword(u.User.Username(), password)
	return u.String(), nil
}

func databaseHint(defaultDB string) string {
	if defaultDB == "" {
		return " --database <db>"
	}
	return ""
}
