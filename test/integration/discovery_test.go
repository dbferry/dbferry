//go:build integration

package integration

import (
	"context"
	"testing"

	"github.com/dbferry/dbferry/pipeline"
)

func findDB(dbs []pipeline.DatabaseInfo, name string) (pipeline.DatabaseInfo, bool) {
	for _, d := range dbs {
		if d.Name == name {
			return d, true
		}
	}
	return pipeline.DatabaseInfo{}, false
}

// TestDiscoveryPostgres verifies discovery from one cluster DSN: user databases
// are listed (system ones excluded), and a database the connecting role can't
// reach is reported as inaccessible rather than dropped (poc-plan 5.2).
func TestDiscoveryPostgres(t *testing.T) {
	suffix := uniqueSuffix()
	visible := "disc_vis_" + suffix
	secret := "disc_sec_" + suffix
	role := "disc_role_" + suffix

	admin := openPG(t, pg17DSN)
	pgExec(t, admin,
		`CREATE DATABASE "`+visible+`"`,
		`CREATE DATABASE "`+secret+`"`,
		`CREATE ROLE "`+role+`" LOGIN PASSWORD 'p'`,
		`REVOKE CONNECT ON DATABASE "`+secret+`" FROM PUBLIC`,
	)
	t.Cleanup(func() {
		admin.Exec(`DROP DATABASE IF EXISTS "` + visible + `" WITH (FORCE)`)
		admin.Exec(`DROP DATABASE IF EXISTS "` + secret + `" WITH (FORCE)`)
		admin.Exec(`DROP ROLE IF EXISTS "` + role + `"`)
	})

	// Connect as the limited role to a database it can access.
	limitedDSN := "postgres://" + role + ":p@localhost:5417/" + visible
	if err := pipeline.TestConnection(context.Background(), limitedDSN); err != nil {
		t.Fatalf("test-connection: %v", err)
	}

	dbs, err := pipeline.ListDatabases(context.Background(), limitedDSN)
	if err != nil {
		t.Fatalf("list databases: %v", err)
	}
	if v, ok := findDB(dbs, visible); !ok || !v.Accessible {
		t.Errorf("visible database should be listed and accessible: %+v (found=%v)", v, ok)
	}
	if s, ok := findDB(dbs, secret); !ok || s.Accessible {
		t.Errorf("no-connect database should be listed but inaccessible: %+v (found=%v)", s, ok)
	}
	for _, sys := range []string{"postgres", "template0", "template1"} {
		if _, ok := findDB(dbs, sys); ok {
			t.Errorf("system database %q must be filtered out", sys)
		}
	}
}

// TestDiscoveryMySQL: user schemas are listed, system schemas excluded.
func TestDiscoveryMySQL(t *testing.T) {
	suffix := uniqueSuffix()
	dbName := "disc_my_" + suffix
	runMySQL(t, "-e", "CREATE DATABASE "+dbName)
	t.Cleanup(func() { runMySQL(t, "-e", "DROP DATABASE IF EXISTS "+dbName) })

	dsn := mysqlURL(dbName)
	if err := pipeline.TestConnection(context.Background(), dsn); err != nil {
		t.Fatalf("test-connection: %v", err)
	}
	dbs, err := pipeline.ListDatabases(context.Background(), dsn)
	if err != nil {
		t.Fatalf("list databases: %v", err)
	}
	if d, ok := findDB(dbs, dbName); !ok || !d.Accessible {
		t.Errorf("created database should be listed and accessible: found=%v", ok)
	}
	for _, sys := range []string{"mysql", "information_schema", "performance_schema", "sys"} {
		if _, ok := findDB(dbs, sys); ok {
			t.Errorf("system schema %q must be filtered out", sys)
		}
	}
}
