package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/base-al/basepod/internal/gitsource"
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

// TestGitAskpassCommandPrintsTokenForPasswordPrompt proves the hidden
// `internal-git-askpass` subcommand (see internal/gitsource.Fetch's
// GIT_ASKPASS wiring) is actually connected to gitsource.Askpass: given a
// password-style prompt as its one argument, it prints exactly the
// configured deploy token, read from the environment — never anywhere
// else — and nothing more.
func TestGitAskpassCommandPrintsTokenForPasswordPrompt(t *testing.T) {
	t.Setenv(gitsource.GitTokenEnvVar, "test-token-xyz")

	cmd := newGitAskpassCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"Password for 'https://example.com': "})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	got := strings.TrimSpace(out.String())
	if got != "test-token-xyz" {
		t.Fatalf("got %q, want the token and nothing else", got)
	}
}

// TestGitAskpassCommandIsHidden proves the subcommand doesn't show up in
// `basepod --help` — it's an internal implementation detail of git
// credential transport, not something an operator should run by hand.
func TestGitAskpassCommandIsHidden(t *testing.T) {
	cmd := newGitAskpassCmd()
	if !cmd.Hidden {
		t.Fatal("internal-git-askpass command must be Hidden")
	}
}
