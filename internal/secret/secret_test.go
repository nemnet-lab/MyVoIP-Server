package secret

import (
	"strings"
	"testing"
)

func TestBoxRoundTrip(t *testing.T) {
	box, err := NewBox("MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY=")
	if err != nil {
		t.Fatal(err)
	}
	sealed := box.Seal("pässword")
	if strings.Contains(string(sealed), "pässword") {
		t.Fatal("not encrypted")
	}
	plain, err := box.Open(sealed)
	if err != nil || plain != "pässword" {
		t.Fatalf("%v %q", err, plain)
	}
	other, _ := NewBox("ZmVkY2JhOTg3NjU0MzIxMGZlZGNiYTk4NzY1NDMyMTA=")
	if _, err := other.Open(sealed); err == nil {
		t.Fatal("wrong key must fail")
	}
}

func TestNewKeyIsUsable(t *testing.T) {
	if _, err := NewBox(NewKey()); err != nil {
		t.Fatal(err)
	}
}

func TestBoxRejectsShortKey(t *testing.T) {
	if _, err := NewBox("c2hvcnQ="); err == nil {
		t.Fatal("short key accepted")
	}
}

func TestEnrollmentCode(t *testing.T) {
	code := NewEnrollmentCode()
	if len(code) != 8 || strings.ContainsAny(code, "01OIL") {
		t.Fatalf("bad code %q", code)
	}
	if NormalizeCode(" "+strings.ToLower(FormatCode(code))+" ") != code {
		t.Fatal("normalize mismatch")
	}
}

func TestNewIDIsUUIDv4(t *testing.T) {
	id := NewID()
	if len(id) != 36 || id[14] != '4' {
		t.Fatalf("bad id %s", id)
	}
}
