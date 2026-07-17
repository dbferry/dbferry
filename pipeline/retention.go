package pipeline

import (
	"fmt"
	"sort"
	"time"
)

// RetentionPolicy is a GFS (grandfather-father-son) policy: keep the newest
// backup of each of the N most recent distinct UTC days / ISO weeks / months
// that contain backups. Buckets are counted over backups that exist, not over
// the calendar — the passage of time alone never deletes anything; a gap in
// backups widens the kept window instead of draining it.
//
// The JSON field names are a public contract (stored by the cloud service,
// accepted by the CLI).
type RetentionPolicy struct {
	KeepDaily   int `json:"keep_daily"`
	KeepWeekly  int `json:"keep_weekly"`
	KeepMonthly int `json:"keep_monthly"`
}

// maxKeep bounds each policy counter; 3650 daily backups is a decade.
const maxKeep = 3650

// Validate rejects a policy that is out of bounds or would keep nothing.
func (p RetentionPolicy) Validate() error {
	for _, v := range []struct {
		name string
		n    int
	}{{"keep_daily", p.KeepDaily}, {"keep_weekly", p.KeepWeekly}, {"keep_monthly", p.KeepMonthly}} {
		if v.n < 0 || v.n > maxKeep {
			return fmt.Errorf("pipeline: retention policy: %s must be between 0 and %d, got %d", v.name, maxKeep, v.n)
		}
	}
	if p.KeepDaily == 0 && p.KeepWeekly == 0 && p.KeepMonthly == 0 {
		return fmt.Errorf("pipeline: retention policy keeps nothing: set at least one of keep_daily, keep_weekly, keep_monthly")
	}
	return nil
}

// SelectRetention partitions the valid backups into those the policy keeps and
// those it drops. It is pure and deterministic: only BackupValid entries are
// considered (every other state is excluded from both sets), and the newest
// valid backup is always kept — a retention pass can never delete the last
// valid backup. An all-zero policy keeps everything (defense in depth;
// Validate rejects it upstream).
func SelectRetention(backups []BackupInfo, policy RetentionPolicy) (keep, drop []BackupInfo) {
	valid := sortedValid(backups)
	if len(valid) == 0 {
		return nil, nil
	}

	keepSet := map[string]bool{valid[0].Key: true} // the newest always survives
	if policy.KeepDaily == 0 && policy.KeepWeekly == 0 && policy.KeepMonthly == 0 {
		return valid, nil
	}
	mark := func(n int, bucket func(time.Time) string) {
		if n <= 0 {
			return
		}
		seen := map[string]bool{}
		for _, b := range valid { // newest first ⇒ the newest per bucket wins
			k := bucket(b.CreatedAt.UTC())
			if seen[k] {
				continue
			}
			seen[k] = true
			keepSet[b.Key] = true
			if len(seen) == n {
				return
			}
		}
	}
	mark(policy.KeepDaily, func(t time.Time) string { return t.Format("2006-01-02") })
	mark(policy.KeepWeekly, func(t time.Time) string {
		y, w := t.ISOWeek()
		return fmt.Sprintf("%04d-W%02d", y, w)
	})
	mark(policy.KeepMonthly, func(t time.Time) string { return t.Format("2006-01") })

	for _, b := range valid {
		if keepSet[b.Key] {
			keep = append(keep, b)
		} else {
			drop = append(drop, b)
		}
	}
	return keep, drop
}

// sortedValid filters to BackupValid and orders newest first (key as a
// deterministic tie-break), without mutating the input.
func sortedValid(backups []BackupInfo) []BackupInfo {
	var valid []BackupInfo
	for _, b := range backups {
		if b.State == BackupValid {
			valid = append(valid, b)
		}
	}
	sort.Slice(valid, func(i, j int) bool {
		if !valid[i].CreatedAt.Equal(valid[j].CreatedAt) {
			return valid[i].CreatedAt.After(valid[j].CreatedAt)
		}
		return valid[i].Key > valid[j].Key
	})
	return valid
}
