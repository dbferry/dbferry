package main

import (
	"flag"
	"fmt"
	"io"
	"net/url"
	"strings"

	"github.com/dbferry/dbferry/config"
)

// configPath resolves the config file location (honouring --config via the
// caller-parsed value, else the default).
func configPath(override string) string {
	if override != "" {
		return override
	}
	return config.DefaultPath()
}

// splitName takes the leading positional <name> then parses the remaining
// flags, so commands read naturally as `add <name> --flags` (Go's flag package
// otherwise stops at the first positional).
func splitName(args []string, fs *flag.FlagSet) (name string, ok bool) {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return "", false
	}
	if err := fs.Parse(args[1:]); err != nil {
		return "", false
	}
	return args[0], true
}

// --- connections ----------------------------------------------------------

func cmdConnections(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: dbferry connections <list|show|add|rm> ...")
		return 1
	}
	switch args[0] {
	case "list":
		return connectionsList(args[1:], stdout, stderr)
	case "show":
		return connectionsShow(args[1:], stdout, stderr)
	case "add":
		return connectionsAdd(args[1:], stdout, stderr)
	case "rm", "remove":
		return connectionsRm(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "dbferry: unknown connections subcommand %q\n", args[0])
		return 1
	}
}

func connectionsList(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("connections list", stderr)
	cfgPath := fs.String("config", "", "config file path")
	if err := fs.Parse(args); err != nil {
		return 1
	}
	cfg, err := config.Load(configPath(*cfgPath))
	if err != nil {
		fmt.Fprintln(stderr, "dbferry: "+err.Error())
		return 1
	}
	names := cfg.ConnectionNames()
	if len(names) == 0 {
		fmt.Fprintln(stdout, "no connections (run `dbferry init`)")
		return 0
	}
	for _, n := range names {
		c := cfg.Connections[n]
		fmt.Fprintf(stdout, "  %-20s %-9s → %s\n", n, c.Engine, c.Destination)
	}
	return 0
}

func connectionsShow(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("connections show", stderr)
	cfgPath := fs.String("config", "", "config file path")
	name, ok := splitName(args, fs)
	if !ok {
		fmt.Fprintln(stderr, "usage: dbferry connections show <name>")
		return 1
	}
	cfg, err := config.Load(configPath(*cfgPath))
	if err != nil {
		fmt.Fprintln(stderr, "dbferry: "+err.Error())
		return 1
	}
	c := cfg.Connections[name]
	if c == nil {
		fmt.Fprintf(stderr, "dbferry: no connection named %q\n", name)
		return 1
	}
	// Never print secret values — only the reference.
	fmt.Fprintf(stdout, "engine        %s\ndsn           %s\npassword      %s\ndefault_database %s\ndestination   %s\nage_recipient %s\n",
		c.Engine, c.DSN, c.Password.String(), c.DefaultDatabase, c.Destination, c.AgeRecipient)
	return 0
}

func connectionsAdd(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("connections add", stderr)
	var (
		cfgPath      = fs.String("config", "", "config file path")
		dsn          = fs.String("dsn", "", "full DSN (postgres://... or mysql://...); the password is stripped and stored via the chosen secret backend")
		pwEnv        = fs.String("password-env", "", "store the password as an env reference (env var name); its value is NOT stored")
		pwKeyring    = fs.String("password-keyring", "", "store the password in the OS keychain under this name (value taken from the DSN or stdin)")
		defaultDB    = fs.String("default-database", "", "database to back up when --database is omitted")
		destination  = fs.String("destination", "", "default destination name")
		ageRecipient = fs.String("age-recipient", "", "age public recipient")
	)
	name, ok := splitName(args, fs)
	if !ok {
		fmt.Fprintln(stderr, "usage: dbferry connections add <name> --dsn ... (--password-keyring NAME | --password-env VAR)")
		return 1
	}
	if *dsn == "" {
		fmt.Fprintln(stderr, "dbferry: --dsn is required")
		return 1
	}
	if (*pwEnv == "") == (*pwKeyring == "") {
		fmt.Fprintln(stderr, "dbferry: choose exactly one of --password-keyring or --password-env")
		return 1
	}

	engine, template, password, err := splitDSN(*dsn)
	if err != nil {
		fmt.Fprintln(stderr, "dbferry: "+err.Error())
		return 1
	}

	conn := &config.Connection{
		Engine: engine, DSN: template, DefaultDatabase: *defaultDB,
		Destination: *destination, AgeRecipient: *ageRecipient,
	}
	if *pwEnv != "" {
		conn.Password = config.SecretRef{Env: *pwEnv}
	} else {
		if password == "" {
			password = readSecret(stderr, "database password: ")
		}
		if password == "" {
			fmt.Fprintln(stderr, "dbferry: no password given (put it in the DSN or type it when prompted)")
			return 1
		}
		conn.Password = config.SecretRef{Keyring: *pwKeyring}
	}
	if err := conn.Validate(); err != nil {
		fmt.Fprintf(stderr, "dbferry: invalid connection: %v\n", err)
		return 1
	}

	// Store secret first, then config; roll back the keychain entry if the
	// config write fails, so no orphan secret is left behind.
	if *pwKeyring != "" {
		if err := conn.Password.Store(password); err != nil {
			fmt.Fprintln(stderr, "dbferry: "+err.Error())
			return 1
		}
	}
	if err := upsertConnection(configPath(*cfgPath), name, conn); err != nil {
		if *pwKeyring != "" {
			_ = conn.Password.Delete()
		}
		fmt.Fprintln(stderr, "dbferry: "+err.Error())
		return 1
	}
	fmt.Fprintf(stdout, "added connection %q (%s)\n", name, engine)
	return 0
}

func connectionsRm(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("connections rm", stderr)
	cfgPath := fs.String("config", "", "config file path")
	name, ok := splitName(args, fs)
	if !ok {
		fmt.Fprintln(stderr, "usage: dbferry connections rm <name>")
		return 1
	}
	path := configPath(*cfgPath)
	cfg, err := config.Load(path)
	if err != nil {
		fmt.Fprintln(stderr, "dbferry: "+err.Error())
		return 1
	}
	c := cfg.Connections[name]
	if c == nil {
		fmt.Fprintf(stderr, "dbferry: no connection named %q\n", name)
		return 1
	}
	delete(cfg.Connections, name)
	// Delete the keychain secret only if no other record references it.
	if c.Password.Keyring != "" && !secretRefStillUsed(cfg, c.Password) {
		if err := c.Password.Delete(); err != nil {
			fmt.Fprintf(stderr, "dbferry: warning: %v\n", err)
		}
	}
	if err := cfg.Save(path); err != nil {
		fmt.Fprintln(stderr, "dbferry: "+err.Error())
		return 1
	}
	fmt.Fprintf(stdout, "removed connection %q\n", name)
	return 0
}

// --- destinations ---------------------------------------------------------

func cmdDestinations(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: dbferry destinations <list|show|add|rm> ...")
		return 1
	}
	switch args[0] {
	case "list":
		return destinationsList(args[1:], stdout, stderr)
	case "show":
		return destinationsShow(args[1:], stdout, stderr)
	case "add":
		return destinationsAdd(args[1:], stdout, stderr)
	case "rm", "remove":
		return destinationsRm(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "dbferry: unknown destinations subcommand %q\n", args[0])
		return 1
	}
}

func destinationsList(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("destinations list", stderr)
	cfgPath := fs.String("config", "", "config file path")
	if err := fs.Parse(args); err != nil {
		return 1
	}
	cfg, err := config.Load(configPath(*cfgPath))
	if err != nil {
		fmt.Fprintln(stderr, "dbferry: "+err.Error())
		return 1
	}
	names := cfg.DestinationNames()
	if len(names) == 0 {
		fmt.Fprintln(stdout, "no destinations")
		return 0
	}
	for _, n := range names {
		d := cfg.Destinations[n]
		fmt.Fprintf(stdout, "  %-20s %s\n", n, d.DestURL())
	}
	return 0
}

func destinationsShow(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("destinations show", stderr)
	cfgPath := fs.String("config", "", "config file path")
	name, ok := splitName(args, fs)
	if !ok {
		fmt.Fprintln(stderr, "usage: dbferry destinations show <name>")
		return 1
	}
	cfg, err := config.Load(configPath(*cfgPath))
	if err != nil {
		fmt.Fprintln(stderr, "dbferry: "+err.Error())
		return 1
	}
	d := cfg.Destinations[name]
	if d == nil {
		fmt.Fprintf(stderr, "dbferry: no destination named %q\n", name)
		return 1
	}
	fmt.Fprintf(stdout, "bucket        %s\nprefix        %s\nendpoint      %s\nregion        %s\nprofile       %s\naccess_key    %s\nsecret_key    %s\nsession_token %s\n",
		d.Bucket, d.Prefix, d.Endpoint, d.Region, d.Profile,
		refString(d.AccessKey), refString(d.SecretKey), refString(d.SessionToken))
	return 0
}

func destinationsAdd(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("destinations add", stderr)
	var (
		cfgPath  = fs.String("config", "", "config file path")
		bucket   = fs.String("bucket", "", "S3 bucket / Space name (required)")
		prefix   = fs.String("prefix", "", "key prefix")
		endpoint = fs.String("endpoint", "", "S3-compatible endpoint (empty for AWS S3)")
		region   = fs.String("region", "", "region")
		profile  = fs.String("profile", "", "AWS shared-config profile (instead of static keys)")
		akEnv    = fs.String("access-key-env", "", "access key as an env reference")
		skEnv    = fs.String("secret-key-env", "", "secret key as an env reference")
		stEnv    = fs.String("session-token-env", "", "session token as an env reference (STS)")
	)
	name, ok := splitName(args, fs)
	if !ok {
		fmt.Fprintln(stderr, "usage: dbferry destinations add <name> --bucket ... [--endpoint ... --region ... --access-key-env ... --secret-key-env ...]")
		return 1
	}
	if *bucket == "" {
		fmt.Fprintln(stderr, "dbferry: --bucket is required")
		return 1
	}
	dst := &config.Destination{
		Bucket: *bucket, Prefix: *prefix, Endpoint: *endpoint, Region: *region, Profile: *profile,
	}
	if *akEnv != "" {
		dst.AccessKey = &config.SecretRef{Env: *akEnv}
	}
	if *skEnv != "" {
		dst.SecretKey = &config.SecretRef{Env: *skEnv}
	}
	if *stEnv != "" {
		dst.SessionToken = &config.SecretRef{Env: *stEnv}
	}
	if err := dst.Validate(); err != nil {
		fmt.Fprintf(stderr, "dbferry: invalid destination: %v\n", err)
		return 1
	}
	path := configPath(*cfgPath)
	cfg, err := config.Load(path)
	if err != nil {
		fmt.Fprintln(stderr, "dbferry: "+err.Error())
		return 1
	}
	cfg.Destinations[name] = dst
	if err := cfg.Save(path); err != nil {
		fmt.Fprintln(stderr, "dbferry: "+err.Error())
		return 1
	}
	fmt.Fprintf(stdout, "added destination %q → %s\n", name, dst.DestURL())
	return 0
}

func destinationsRm(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("destinations rm", stderr)
	cfgPath := fs.String("config", "", "config file path")
	name, ok := splitName(args, fs)
	if !ok {
		fmt.Fprintln(stderr, "usage: dbferry destinations rm <name>")
		return 1
	}
	path := configPath(*cfgPath)
	cfg, err := config.Load(path)
	if err != nil {
		fmt.Fprintln(stderr, "dbferry: "+err.Error())
		return 1
	}
	d := cfg.Destinations[name]
	if d == nil {
		fmt.Fprintf(stderr, "dbferry: no destination named %q\n", name)
		return 1
	}
	delete(cfg.Destinations, name)
	// Delete keychain-backed dest secrets only if not shared.
	for _, r := range []*config.SecretRef{d.AccessKey, d.SecretKey, d.SessionToken} {
		if r != nil && r.Keyring != "" && !secretRefStillUsed(cfg, *r) {
			if err := r.Delete(); err != nil {
				fmt.Fprintf(stderr, "dbferry: warning: %v\n", err)
			}
		}
	}
	if err := cfg.Save(path); err != nil {
		fmt.Fprintln(stderr, "dbferry: "+err.Error())
		return 1
	}
	fmt.Fprintf(stdout, "removed destination %q\n", name)
	return 0
}

func refString(r *config.SecretRef) string {
	if r == nil {
		return "(none → AWS default chain)"
	}
	return r.String()
}

// --- helpers --------------------------------------------------------------

// splitDSN separates a full DSN into engine, a password-stripped template and
// the password (if any).
func splitDSN(dsn string) (engine, template, password string, err error) {
	u, err := url.Parse(dsn)
	if err != nil {
		return "", "", "", fmt.Errorf("invalid DSN URL")
	}
	switch u.Scheme {
	case "postgres", "postgresql":
		engine = "postgres"
	case "mysql":
		engine = "mysql"
	default:
		return "", "", "", fmt.Errorf("DSN scheme %q must be postgres:// or mysql://", u.Scheme)
	}
	if pw, ok := u.User.Password(); ok {
		password = pw
		u.User = url.User(u.User.Username()) // strip password
	}
	return engine, u.String(), password, nil
}

func upsertConnection(path, name string, conn *config.Connection) error {
	cfg, err := config.Load(path)
	if err != nil {
		return err
	}
	cfg.Connections[name] = conn
	return cfg.Save(path)
}

func secretRefStillUsed(cfg *config.Config, ref config.SecretRef) bool {
	for _, c := range cfg.Connections {
		if c.Password.Keyring == ref.Keyring {
			return true
		}
	}
	for _, d := range cfg.Destinations {
		for _, r := range []*config.SecretRef{d.AccessKey, d.SecretKey, d.SessionToken} {
			if r != nil && r.Keyring == ref.Keyring {
				return true
			}
		}
	}
	return false
}

// readSecret reads a secret from the terminal without echo, or from stdin if it
// is piped.
func readSecret(prompt io.Writer, label string) string {
	fmt.Fprint(prompt, label)
	return readLineNoEcho()
}
