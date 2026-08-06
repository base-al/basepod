package main

import (
	"bytes"
	"strings"
	"testing"
)

// TestWarnAdminPasswordFlagWarnsWhenUsed proves warnAdminPasswordFlag
// prints a warning when --admin-password was actually supplied (audit
// finding L4) — the flag itself is kept (removing it would break the
// documented `basepod setup` flow), but every use should tell the
// operator it's visible via `ps` and shell history.
func TestWarnAdminPasswordFlagWarnsWhenUsed(t *testing.T) {
	var buf bytes.Buffer
	warnAdminPasswordFlag(&buf, "hunter2")

	if buf.Len() == 0 {
		t.Fatal("expected a warning to be written when --admin-password is used")
	}
	if !strings.Contains(buf.String(), "admin-password") {
		t.Fatalf("warning = %q, want it to mention --admin-password", buf.String())
	}
	if strings.Contains(buf.String(), "hunter2") {
		t.Fatalf("warning leaked the actual password: %q", buf.String())
	}
}

// TestWarnAdminPasswordFlagSilentWhenEmpty proves warnAdminPasswordFlag
// writes nothing when the flag wasn't actually supplied (an empty
// password) — the required-flag check in newSetupCmd already fails the
// command in that case, so this is defense against the warning firing
// spuriously if the call order in newSetupCmd ever changes.
func TestWarnAdminPasswordFlagSilentWhenEmpty(t *testing.T) {
	var buf bytes.Buffer
	warnAdminPasswordFlag(&buf, "")

	if buf.Len() != 0 {
		t.Fatalf("expected no output for an empty password, got %q", buf.String())
	}
}
