package pipeline

import (
	"context"
	"database/sql"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"strings"

	"github.com/go-sql-driver/mysql"
)

// mysqlSource is a MySQL connection resolved from the DSN. The password reaches
// mysqldump through the MYSQL_PWD environment variable, never on argv.
type mysqlSource struct {
	host     string
	port     string
	user     string
	password string
	database string
	// sslMode is the normalized MySQL ssl-mode from the DSN query (REQUIRED,
	// VERIFY_CA, ...); empty = not specified (driver/tool defaults). Managed
	// providers create users with REQUIRE SSL — dropping this turns into a
	// misleading 1045 Access denied even with the right password.
	sslMode string
}

// mysqlSSLModes maps the DSN's ssl-mode (MySQL client semantics) onto
// go-sql-driver's tls parameter. REQUIRED encrypts without verifying the
// server certificate — exactly MySQL's meaning; verification starts at
// VERIFY_CA ("true" also checks the hostname, i.e. VERIFY_IDENTITY — the
// stricter reading is the safe one without a custom CA pool).
var mysqlSSLModes = map[string]string{
	"":                "", // not specified — driver default
	"DISABLED":        "false",
	"PREFERRED":       "preferred",
	"REQUIRED":        "skip-verify",
	"VERIFY_CA":       "true",
	"VERIFY_IDENTITY": "true",
}

func parseMySQLDSN(dsn string) (mysqlSource, error) {
	u, err := url.Parse(dsn)
	if err != nil {
		return mysqlSource{}, classify(KindConnect, "pipeline: parse DSN: invalid URL")
	}
	if u.Scheme != "mysql" {
		return mysqlSource{}, classify(KindConnect, "pipeline: DSN scheme %q is not a MySQL URL", u.Scheme)
	}
	db := strings.TrimPrefix(u.Path, "/")
	if db == "" {
		return mysqlSource{}, classify(KindConnect, "pipeline: DSN must include a database name (mysql://host/DBNAME)")
	}
	port := u.Port()
	if port == "" {
		port = "3306"
	}
	sslMode := strings.ToUpper(u.Query().Get("ssl-mode"))
	if _, ok := mysqlSSLModes[sslMode]; !ok {
		return mysqlSource{}, classify(KindConnect,
			"pipeline: unsupported ssl-mode %q (use DISABLED, PREFERRED, REQUIRED, VERIFY_CA or VERIFY_IDENTITY)", sslMode)
	}
	pw, _ := u.User.Password()
	return mysqlSource{
		host:     u.Hostname(),
		port:     port,
		user:     u.User.Username(),
		password: pw,
		database: db,
		sslMode:  sslMode,
	}, nil
}

// goDSN builds the go-sql-driver DSN, using mysql.Config so credentials with
// special characters are escaped correctly.
func (s mysqlSource) goDSN() string {
	cfg := mysql.NewConfig()
	cfg.User = s.user
	cfg.Passwd = s.password
	cfg.Net = "tcp"
	cfg.Addr = net.JoinHostPort(s.host, s.port)
	cfg.DBName = s.database
	if tls := mysqlSSLModes[s.sslMode]; tls != "" {
		cfg.TLSConfig = tls
	}
	return cfg.FormatDSN()
}

// sslArgs are the --ssl-mode flags for mysqldump/mysql, mirroring what the
// driver connection uses so the dump can't fail where the probe succeeded.
func (s mysqlSource) sslArgs() []string {
	if s.sslMode == "" {
		return nil
	}
	return []string{"--ssl-mode=" + s.sslMode}
}

func (s mysqlSource) cluster() string {
	c := s.host
	if s.port != "3306" {
		c += "_" + s.port
	}
	return sanitizeKeySegment(c)
}

// mysqlDriver backs up a MySQL database with mysqldump.
type mysqlDriver struct {
	src mysqlSource
}

func newMySQLDriver(dsn string) (*mysqlDriver, error) {
	src, err := parseMySQLDSN(dsn)
	if err != nil {
		return nil, err
	}
	return &mysqlDriver{src: src}, nil
}

func (d *mysqlDriver) Engine() string   { return "mysql" }
func (d *mysqlDriver) Cluster() string  { return d.src.cluster() }
func (d *mysqlDriver) Database() string { return d.src.database }

func (d *mysqlDriver) TestConnection(ctx context.Context) error {
	db, err := sql.Open("mysql", d.src.goDSN())
	if err != nil {
		return classify(KindConnect, "pipeline: open mysql: %w", err)
	}
	defer db.Close()
	if err := db.PingContext(ctx); err != nil {
		return classify(KindConnect, "pipeline: connect to database: %w", err)
	}
	return nil
}

// Preflight refuses to back up non-InnoDB tables unless explicitly allowed:
// --single-transaction gives them no consistent snapshot, so a silent success
// would be a lie about backup integrity (poc-plan 4.2).
func (d *mysqlDriver) Preflight(ctx context.Context, opts DriverOptions) error {
	db, err := sql.Open("mysql", d.src.goDSN())
	if err != nil {
		return classify(KindConnect, "pipeline: open mysql: %w", err)
	}
	defer db.Close()

	rows, err := db.QueryContext(ctx,
		`SELECT table_name, engine FROM information_schema.tables
		 WHERE table_schema = ? AND table_type = 'BASE TABLE'
		   AND engine IS NOT NULL AND engine <> 'InnoDB'
		 ORDER BY table_name`, d.src.database)
	if err != nil {
		return classify(KindConnect, "pipeline: detect table engines: %w", err)
	}
	defer rows.Close()

	var nonInnoDB []string
	for rows.Next() {
		var name, engine string
		if err := rows.Scan(&name, &engine); err != nil {
			return err
		}
		nonInnoDB = append(nonInnoDB, fmt.Sprintf("%s (%s)", name, engine))
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(nonInnoDB) == 0 {
		return nil
	}

	list := strings.Join(nonInnoDB, ", ")
	if !opts.AllowNonTransactional {
		return classify(KindDump,
			"pipeline: database %q has non-InnoDB tables that --single-transaction cannot snapshot consistently: %s. "+
				"Re-run with --allow-nontransactional to back them up anyway (they may be inconsistent if written during the backup).",
			d.src.database, list)
	}
	if opts.Warn != nil {
		opts.Warn(fmt.Sprintf("non-InnoDB tables (%s) may be inconsistent under --single-transaction; proceeding due to --allow-nontransactional", list))
	}
	return nil
}

func (d *mysqlDriver) ListDatabases(ctx context.Context) ([]DatabaseInfo, error) {
	db, err := sql.Open("mysql", d.src.goDSN())
	if err != nil {
		return nil, classify(KindConnect, "pipeline: open mysql: %w", err)
	}
	defer db.Close()
	// information_schema.schemata only lists schemas the role can see, so every
	// returned database is accessible; the system schemas are filtered out.
	rows, err := db.QueryContext(ctx,
		`SELECT schema_name FROM information_schema.schemata
		 WHERE schema_name NOT IN ('information_schema','mysql','performance_schema','sys')
		 ORDER BY schema_name`)
	if err != nil {
		return nil, classify(KindConnect, "pipeline: list databases: %w", err)
	}
	defer rows.Close()
	var dbs []DatabaseInfo
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		dbs = append(dbs, DatabaseInfo{Name: name, Accessible: true})
	}
	return dbs, rows.Err()
}

// BuildDumpCommand builds mysqldump for a single database. --single-transaction
// gives a consistent InnoDB snapshot; --set-gtid-purged=OFF keeps the dump
// restorable into any server; --routines and --events include stored programs
// and events (triggers travel with their tables and are on by default, made
// explicit here). The password goes through MYSQL_PWD, never argv.
func (d *mysqlDriver) BuildDumpCommand(ctx context.Context) *exec.Cmd {
	args := []string{
		"-h", d.src.host, "-P", d.src.port, "-u", d.src.user, "--protocol=TCP",
	}
	args = append(args, d.src.sslArgs()...)
	args = append(args,
		"--single-transaction",
		"--set-gtid-purged=OFF",
		"--routines",
		"--events",
		"--triggers",
		d.src.database,
	)
	cmd := exec.CommandContext(ctx, "mysqldump", args...)
	cmd.Env = append(os.Environ(), "MYSQL_PWD="+d.src.password)
	return cmd
}

func (d *mysqlDriver) BuildRestoreCommand(targetDB string) []string {
	args := []string{"mysql", "-h", d.src.host, "-P", d.src.port, "-u", d.src.user, "--protocol=TCP"}
	args = append(args, d.src.sslArgs()...)
	return append(args, targetDB)
}

func (d *mysqlDriver) DumpFormat() string {
	return "mysqldump --single-transaction --set-gtid-purged=OFF --routines --events | zstd | age"
}

func (d *mysqlDriver) DumpClientVersion(ctx context.Context) string {
	out, err := exec.CommandContext(ctx, "mysqldump", "--version").Output()
	if err != nil {
		return "unknown"
	}
	return strings.TrimSpace(string(out))
}

func (d *mysqlDriver) Diagnose(ctx context.Context) []Check {
	if _, err := exec.LookPath("mysqldump"); err != nil {
		return []Check{{Name: "mysqldump client", Status: StatusFail, Detail: "not found on PATH",
			Fix: "install the MySQL client tools (mysql-client / mysql-community-client)"}}
	}
	return []Check{{Name: "mysqldump client", Status: StatusOK, Detail: d.DumpClientVersion(ctx)}}
}
