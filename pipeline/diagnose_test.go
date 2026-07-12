package pipeline

import (
	"context"
	"errors"
	"testing"
)

func TestCheckStatusString(t *testing.T) {
	cases := map[CheckStatus]string{StatusOK: "ok", StatusWarn: "warn", StatusFail: "fail", CheckStatus(99): "unknown"}
	for s, want := range cases {
		if s.String() != want {
			t.Errorf("CheckStatus(%d) = %q, want %q", s, s.String(), want)
		}
	}
}

func TestErrStr(t *testing.T) {
	if errStr(nil) != "" {
		t.Error("nil error should be empty string")
	}
	if errStr(errors.New("boom")) != "boom" {
		t.Error("error string")
	}
}

func TestMySQLDiagnose(t *testing.T) {
	d, err := newMySQLDriver("mysql://u@h:3306/db")
	if err != nil {
		t.Fatal(err)
	}
	checks := d.Diagnose(context.Background())
	if len(checks) != 1 || checks[0].Name != "mysqldump client" {
		t.Fatalf("mysql Diagnose = %+v", checks)
	}
	// Status depends on whether mysqldump is on PATH; both outcomes are valid.
	if checks[0].Status == StatusFail && checks[0].Fix == "" {
		t.Error("a failing client check should carry a fix")
	}
}
