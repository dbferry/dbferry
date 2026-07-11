package pipeline

import (
	"errors"
	"testing"
)

func TestKindOf(t *testing.T) {
	if got := KindOf(errors.New("plain")); got != KindUnknown {
		t.Errorf("plain error kind = %v, want unknown", got)
	}
	e := classify(KindUpload, "boom: %w", errors.New("x"))
	if KindOf(e) != KindUpload {
		t.Errorf("kind = %v, want upload", KindOf(e))
	}
	// The most specific classification (closest to the cause) must win when a
	// classified error is wrapped again.
	wrapped := classify(KindConnect, "outer: %w", e)
	if KindOf(wrapped) != KindUpload {
		t.Errorf("wrapped kind = %v, want upload (existing kind wins)", KindOf(wrapped))
	}
}

func TestDumpFailureIsKindDump(t *testing.T) {
	err := dumpFailure(errors.New("exit status 1"), "pg_dump: permission denied")
	if KindOf(err) != KindDump {
		t.Errorf("dumpFailure kind = %v, want dump", KindOf(err))
	}
	if !errors.Is(err, err) { // sanity: it is an error
		t.Fatal("nil error")
	}
}

func TestKindString(t *testing.T) {
	cases := map[Kind]string{
		KindUnknown: "unknown", KindConnect: "connect",
		KindDump: "dump", KindUpload: "upload", KindCanceled: "canceled",
	}
	for k, want := range cases {
		if k.String() != want {
			t.Errorf("Kind(%d).String() = %q, want %q", k, k.String(), want)
		}
	}
}
