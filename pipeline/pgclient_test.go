package pipeline

import "testing"

func TestParsePgDumpMajor(t *testing.T) {
	cases := map[string]int{
		"pg_dump (PostgreSQL) 17.2":                        17,
		"pg_dump (PostgreSQL) 14.12 (Debian 14.12-1.pgdg)": 14,
		"pg_dump (PostgreSQL) 18.3":                        18,
	}
	for in, want := range cases {
		got, ok := parsePgDumpMajor(in)
		if !ok || got != want {
			t.Errorf("parsePgDumpMajor(%q) = %d,%v want %d", in, got, ok, want)
		}
	}
	if _, ok := parsePgDumpMajor("no version here"); ok {
		t.Error("expected failure on version-less string")
	}
}

func TestSelectPgDump(t *testing.T) {
	clients := []pgClient{
		{"pg_dump-14", 14}, {"pg_dump-16", 16}, {"pg_dump-17", 17},
	}
	// Exact match preferred.
	if c, err := selectPgDump(clients, 16); err != nil || c.path != "pg_dump-16" {
		t.Errorf("server 16 → %v, %v; want pg_dump-16", c, err)
	}
	// No exact match → smallest newer.
	if c, err := selectPgDump(clients, 15); err != nil || c.path != "pg_dump-16" {
		t.Errorf("server 15 → %v, %v; want pg_dump-16 (smallest newer)", c, err)
	}
	// Exact match at the edges.
	if c, err := selectPgDump(clients, 14); err != nil || c.major != 14 {
		t.Errorf("server 14 → %v, %v; want major 14", c, err)
	}
	// Server newer than every client → error.
	if _, err := selectPgDump(clients, 18); err == nil {
		t.Error("server 18 with clients ≤17 should error")
	}
}

func TestCheckSupportedPGMajor(t *testing.T) {
	for _, m := range []int{14, 15, 16, 17} {
		if err := checkSupportedPGMajor(m); err != nil {
			t.Errorf("major %d should be supported: %v", m, err)
		}
	}
	for _, m := range []int{13, 18, 12} {
		if err := checkSupportedPGMajor(m); err == nil {
			t.Errorf("major %d should be unsupported", m)
		}
	}
}
