package pipeline

import "testing"

func TestNewDriverDispatch(t *testing.T) {
	pg, err := newDriver("postgres://u@h/db")
	if err != nil || pg.Engine() != "postgres" {
		t.Fatalf("postgres dispatch: engine=%v err=%v", pg, err)
	}
	pgl, err := newDriver("postgresql://u@h/db")
	if err != nil || pgl.Engine() != "postgres" {
		t.Fatalf("postgresql dispatch: err=%v", err)
	}
	my, err := newDriver("mysql://u@h/db")
	if err != nil || my.Engine() != "mysql" {
		t.Fatalf("mysql dispatch: engine=%v err=%v", my, err)
	}
	_, err = newDriver("redis://x/y")
	if err == nil || KindOf(err) != KindConnect {
		t.Errorf("unknown scheme should be KindConnect error, got %v", err)
	}
}

func TestBuildRestoreCommands(t *testing.T) {
	pg, _ := newDriver("postgres://u@h/db")
	if got := pg.BuildRestoreCommand("target"); got[0] != "pg_restore" {
		t.Errorf("pg restore = %v", got)
	}
	my, _ := newDriver("mysql://u@h/db")
	if got := my.BuildRestoreCommand("target"); got[0] != "mysql" {
		t.Errorf("mysql restore = %v", got)
	}
}
