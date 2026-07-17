package pipeline

import (
	"testing"
	"time"
)

// vb builds a valid backup created at the given RFC3339 time.
func vb(t *testing.T, created string) BackupInfo {
	t.Helper()
	ts, err := time.Parse(time.RFC3339, created)
	if err != nil {
		t.Fatalf("bad test timestamp %q: %v", created, err)
	}
	key := "p/postgres/host/db/x/" + ts.Format("20060102T150405Z") + "-X" + ciphertextSuffix
	return BackupInfo{Key: key, ManifestKey: manifestKey(key), State: BackupValid, CreatedAt: ts}
}

func keys(bs []BackupInfo) []string {
	out := make([]string, len(bs))
	for i, b := range bs {
		out[i] = b.Key
	}
	return out
}

func TestRetentionPolicyValidate(t *testing.T) {
	cases := []struct {
		name   string
		policy RetentionPolicy
		ok     bool
	}{
		{"typical", RetentionPolicy{KeepDaily: 7, KeepWeekly: 4, KeepMonthly: 6}, true},
		{"daily only", RetentionPolicy{KeepDaily: 1}, true},
		{"zero", RetentionPolicy{}, false},
		{"negative", RetentionPolicy{KeepDaily: -1}, false},
		{"too large", RetentionPolicy{KeepWeekly: maxKeep + 1}, false},
		{"at bound", RetentionPolicy{KeepMonthly: maxKeep}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.policy.Validate(); (err == nil) != tc.ok {
				t.Fatalf("Validate(%+v) = %v, want ok=%v", tc.policy, err, tc.ok)
			}
		})
	}
}

func TestSelectRetention(t *testing.T) {
	daily := func(n int) RetentionPolicy { return RetentionPolicy{KeepDaily: n} }

	cases := []struct {
		name     string
		backups  []BackupInfo
		policy   RetentionPolicy
		wantKeep []string // by created-at, order-agnostic check below uses sets
		wantDrop []string
	}{
		{
			name: "empty set",
		},
		{
			name:     "single backup survives any policy",
			backups:  []BackupInfo{vb(t, "2026-07-01T02:00:00Z")},
			policy:   daily(1),
			wantKeep: []string{"2026-07-01T02:00:00Z"},
		},
		{
			name: "daily keeps newest per day",
			backups: []BackupInfo{
				vb(t, "2026-07-03T02:00:00Z"), vb(t, "2026-07-03T01:00:00Z"),
				vb(t, "2026-07-02T02:00:00Z"),
				vb(t, "2026-07-01T02:00:00Z"),
			},
			policy:   daily(2),
			wantKeep: []string{"2026-07-03T02:00:00Z", "2026-07-02T02:00:00Z"},
			wantDrop: []string{"2026-07-03T01:00:00Z", "2026-07-01T02:00:00Z"},
		},
		{
			name: "gaps widen the window instead of draining it (restic semantics)",
			backups: []BackupInfo{
				vb(t, "2026-07-15T02:00:00Z"),
				vb(t, "2026-05-01T02:00:00Z"), // months-old, still the 2nd non-empty day
				vb(t, "2026-04-30T02:00:00Z"),
			},
			policy:   daily(3),
			wantKeep: []string{"2026-07-15T02:00:00Z", "2026-05-01T02:00:00Z", "2026-04-30T02:00:00Z"},
		},
		{
			name: "one backup can satisfy day, week and month at once",
			backups: []BackupInfo{
				vb(t, "2026-07-15T02:00:00Z"),
				vb(t, "2026-07-14T02:00:00Z"),
			},
			policy:   RetentionPolicy{KeepDaily: 1, KeepWeekly: 1, KeepMonthly: 1},
			wantKeep: []string{"2026-07-15T02:00:00Z"},
			wantDrop: []string{"2026-07-14T02:00:00Z"},
		},
		{
			name: "weekly and monthly reach past the daily window",
			backups: []BackupInfo{
				vb(t, "2026-07-15T02:00:00Z"), // day 1, week 29, month 07
				vb(t, "2026-07-14T02:00:00Z"), // dropped: day window is 1, same week/month covered by newer
				vb(t, "2026-07-08T02:00:00Z"), // week 28
				vb(t, "2026-06-20T02:00:00Z"), // month 06 (week 25 — also 2nd... week window is 2: weeks 29,28)
				vb(t, "2026-05-05T02:00:00Z"), // dropped: months window is 2 (07, 06)
			},
			policy: RetentionPolicy{KeepDaily: 1, KeepWeekly: 2, KeepMonthly: 2},
			wantKeep: []string{
				"2026-07-15T02:00:00Z", "2026-07-08T02:00:00Z", "2026-06-20T02:00:00Z",
			},
			wantDrop: []string{"2026-07-14T02:00:00Z", "2026-05-05T02:00:00Z"},
		},
		{
			name: "non-valid states are excluded from both sets",
			backups: []BackupInfo{
				vb(t, "2026-07-15T02:00:00Z"),
				{Key: "p/orphan" + ciphertextSuffix, State: BackupOrphan, CreatedAt: mustTime(t, "2026-07-16T02:00:00Z")},
				{ManifestKey: "p/dangling" + manifestSuffix, State: BackupDanglingManifest},
				{Key: "p/corrupt" + ciphertextSuffix, State: BackupCorruptManifest},
				{Key: "p/future" + ciphertextSuffix, State: BackupUnsupportedSchema},
			},
			policy:   daily(1),
			wantKeep: []string{"2026-07-15T02:00:00Z"},
		},
		{
			name: "all-zero policy drops nothing (defense in depth)",
			backups: []BackupInfo{
				vb(t, "2026-07-15T02:00:00Z"), vb(t, "2026-07-14T02:00:00Z"), vb(t, "2026-07-13T02:00:00Z"),
			},
			policy:   RetentionPolicy{},
			wantKeep: []string{"2026-07-15T02:00:00Z", "2026-07-14T02:00:00Z", "2026-07-13T02:00:00Z"},
		},
		{
			name: "iso week boundary: sunday and monday are different weeks",
			backups: []BackupInfo{
				vb(t, "2026-07-13T02:00:00Z"), // Monday, ISO week 29
				vb(t, "2026-07-12T02:00:00Z"), // Sunday, ISO week 28
			},
			policy:   RetentionPolicy{KeepWeekly: 2},
			wantKeep: []string{"2026-07-13T02:00:00Z", "2026-07-12T02:00:00Z"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			keep, drop := SelectRetention(tc.backups, tc.policy)
			assertTimes(t, "keep", keep, tc.wantKeep)
			assertTimes(t, "drop", drop, tc.wantDrop)
		})
	}
}

func TestSelectRetentionNewestAlwaysKept(t *testing.T) {
	backups := []BackupInfo{vb(t, "2020-01-01T00:00:00Z"), vb(t, "2019-01-01T00:00:00Z")}
	// Policy windows are all satisfied by... nothing recent — yet the newest
	// valid backup must survive regardless.
	keep, drop := SelectRetention(backups, RetentionPolicy{KeepDaily: 1})
	if len(keep) == 0 || !keep[0].CreatedAt.Equal(mustTime(t, "2020-01-01T00:00:00Z")) {
		t.Fatalf("newest backup not kept: keep=%v drop=%v", keys(keep), keys(drop))
	}
}

func TestSelectRetentionDeterministicTieBreak(t *testing.T) {
	ts := mustTime(t, "2026-07-15T02:00:00Z")
	a := BackupInfo{Key: "p/a" + ciphertextSuffix, State: BackupValid, CreatedAt: ts}
	b := BackupInfo{Key: "p/b" + ciphertextSuffix, State: BackupValid, CreatedAt: ts}
	k1, _ := SelectRetention([]BackupInfo{a, b}, RetentionPolicy{KeepDaily: 1})
	k2, _ := SelectRetention([]BackupInfo{b, a}, RetentionPolicy{KeepDaily: 1})
	if len(k1) != 1 || len(k2) != 1 || k1[0].Key != k2[0].Key {
		t.Fatalf("tie-break is input-order dependent: %v vs %v", keys(k1), keys(k2))
	}
}

func mustTime(t *testing.T, s string) time.Time {
	t.Helper()
	ts, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("bad test timestamp %q: %v", s, err)
	}
	return ts
}

// assertTimes checks that got's creation times equal want (as RFC3339 strings, any order).
func assertTimes(t *testing.T, label string, got []BackupInfo, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: got %d backups %v, want %d %v", label, len(got), keys(got), len(want), want)
	}
	wantSet := map[string]bool{}
	for _, w := range want {
		wantSet[mustTime(t, w).UTC().Format(time.RFC3339)] = true
	}
	for _, g := range got {
		if !wantSet[g.CreatedAt.UTC().Format(time.RFC3339)] {
			t.Fatalf("%s: unexpected backup %s (created %s); want %v", label, g.Key, g.CreatedAt, want)
		}
	}
}
