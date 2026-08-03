package pagecursor

import (
	"testing"
	"time"
)

func TestTimeCursorRoundTrip(t *testing.T) {
	ts := time.Date(2026, 8, 3, 12, 0, 0, 123456789, time.UTC)
	enc := EncodeTime(ts, "run-1")
	got, err := DecodeTime(enc)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Time.Equal(ts) || got.ID != "run-1" {
		t.Fatalf("got %+v", got)
	}
}

func TestKeyCursorRoundTrip(t *testing.T) {
	enc := EncodeKey("echo", "agent-1")
	got, err := DecodeKey(enc)
	if err != nil {
		t.Fatal(err)
	}
	if got.Key != "echo" || got.ID != "agent-1" {
		t.Fatalf("got %+v", got)
	}
}

func TestDecodeEmpty(t *testing.T) {
	if _, err := DecodeTime(""); err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeKey(""); err != nil {
		t.Fatal(err)
	}
}

func TestDecodeRejectsGarbage(t *testing.T) {
	if _, err := DecodeTime("not-a-cursor"); err == nil {
		t.Fatal("expected error")
	}
	if _, err := DecodeKey(EncodeTime(time.Now().UTC(), "x")); err == nil {
		t.Fatal("expected key decode to reject time cursor")
	}
}
