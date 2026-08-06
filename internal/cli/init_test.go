package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestInitPerStack table-tests runInit against a fresh temp dir seeded
// with each stack's marker file(s), asserting the generated
// basepod.yaml and Containerfile carry the expected key lines.
func TestInitPerStack(t *testing.T) {
	tests := []struct {
		name          string
		seed          map[string]string // relative path -> contents
		wantPort      int
		wantCFLines   []string // substrings expected in the generated Containerfile
		wantManifest  []string // substrings expected in the generated basepod.yaml
		containerfile string   // filename runInit is expected to write (default "Containerfile")
	}{
		{
			name:         "node",
			seed:         map[string]string{"package.json": `{"name":"x"}`},
			wantPort:     3000,
			wantCFLines:  []string{"FROM docker.io/library/node:22-alpine", "npm ci", "npm start", "EXPOSE 3000"},
			wantManifest: []string{"port: 3000"},
		},
		{
			name:         "go",
			seed:         map[string]string{"go.mod": "module example.com/x\n\ngo 1.26\n"},
			wantPort:     8080,
			wantCFLines:  []string{"FROM docker.io/library/golang:1.26-alpine AS build", "FROM docker.io/library/alpine:3.22", "CGO_ENABLED=0", "EXPOSE 8080"},
			wantManifest: []string{"port: 8080"},
		},
		{
			name:         "python-requirements",
			seed:         map[string]string{"requirements.txt": "flask\n"},
			wantPort:     8000,
			wantCFLines:  []string{"FROM docker.io/library/python:3.13-alpine", "requirements.txt", "EXPOSE 8000"},
			wantManifest: []string{"port: 8000"},
		},
		{
			name:         "python-pyproject",
			seed:         map[string]string{"pyproject.toml": "[project]\nname = \"x\"\n"},
			wantPort:     8000,
			wantCFLines:  []string{"FROM docker.io/library/python:3.13-alpine", "pip install --no-cache-dir .", "EXPOSE 8000"},
			wantManifest: []string{"port: 8000"},
		},
		{
			name:         "static-index",
			seed:         map[string]string{"index.html": "<html></html>"},
			wantPort:     80,
			wantCFLines:  []string{"FROM docker.io/library/caddy:2.10-alpine", "COPY . /usr/share/caddy", "EXPOSE 80"},
			wantManifest: []string{"port: 80"},
		},
		{
			name:         "generic",
			seed:         map[string]string{"README.md": "nothing detectable here\n"},
			wantPort:     8080,
			wantCFLines:  []string{"FROM docker.io/library/alpine:3.22", "exit 1"},
			wantManifest: []string{"port: 8080"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			for rel, contents := range tt.seed {
				writeFile(t, dir, rel, contents)
			}

			var out bytes.Buffer
			if err := runInit(&out, dir, false, 0, ""); err != nil {
				t.Fatalf("runInit: %v", err)
			}

			manifestData, err := os.ReadFile(filepath.Join(dir, "basepod.yaml"))
			if err != nil {
				t.Fatalf("read basepod.yaml: %v", err)
			}
			for _, want := range tt.wantManifest {
				if !strings.Contains(string(manifestData), want) {
					t.Errorf("basepod.yaml = %q, want it to contain %q", manifestData, want)
				}
			}
			wantName := slugifyName(filepath.Base(dir))
			if !strings.Contains(string(manifestData), "name: "+wantName) {
				t.Errorf("basepod.yaml = %q, want it to contain name: %s", manifestData, wantName)
			}

			cfData, err := os.ReadFile(filepath.Join(dir, "Containerfile"))
			if err != nil {
				t.Fatalf("read Containerfile: %v", err)
			}
			for _, want := range tt.wantCFLines {
				if !strings.Contains(string(cfData), want) {
					t.Errorf("Containerfile = %q, want it to contain %q", cfData, want)
				}
			}

			if !strings.Contains(out.String(), "basepod deploy") {
				t.Errorf("output = %q, want next-steps mentioning `basepod deploy`", out.String())
			}
		})
	}
}

// TestInitNameAndPortFlagsOverrideDetection proves --name and --port
// win over both directory-name-derived defaults and the detected
// stack's conventional port.
func TestInitNameAndPortFlagsOverrideDetection(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "package.json", `{"name":"x"}`)

	var out bytes.Buffer
	if err := runInit(&out, dir, false, 9999, "my-custom-app"); err != nil {
		t.Fatalf("runInit: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dir, "basepod.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "name: my-custom-app") {
		t.Errorf("basepod.yaml = %q, want name: my-custom-app", data)
	}
	if !strings.Contains(string(data), "port: 9999") {
		t.Errorf("basepod.yaml = %q, want port: 9999", data)
	}
}

// TestInitSlugifiesDirectoryName proves a directory name with characters
// the server's slug rules don't allow gets sanitized into a valid slug.
func TestInitSlugifiesDirectoryName(t *testing.T) {
	parent := t.TempDir()
	dir := filepath.Join(parent, "My Cool App!")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := runInit(&out, dir, false, 0, ""); err != nil {
		t.Fatalf("runInit: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "basepod.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "name: my-cool-app") {
		t.Errorf("basepod.yaml = %q, want name: my-cool-app", data)
	}
}

// TestInitRefusesToClobberManifestWithoutForce proves a pre-existing
// basepod.yaml blocks `init` (and nothing else gets written) unless
// --force is passed.
func TestInitRefusesToClobberManifestWithoutForce(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "basepod.yaml", "name: keep-me\nport: 1234\n")

	var out bytes.Buffer
	err := runInit(&out, dir, false, 0, "")
	if err == nil || !strings.Contains(err.Error(), "basepod.yaml") {
		t.Fatalf("err = %v, want a basepod.yaml-already-exists error", err)
	}

	// The existing manifest must be untouched.
	data, rerr := os.ReadFile(filepath.Join(dir, "basepod.yaml"))
	if rerr != nil {
		t.Fatal(rerr)
	}
	if !strings.Contains(string(data), "keep-me") {
		t.Fatalf("basepod.yaml was modified: %q", data)
	}
	// Nothing else should have been written either.
	if _, statErr := os.Stat(filepath.Join(dir, "Containerfile")); statErr == nil {
		t.Fatal("Containerfile was written despite the manifest refusal")
	}
}

// TestInitForceOverwritesManifestAndContainerfile proves --force
// overwrites both an existing basepod.yaml and an existing Containerfile.
func TestInitForceOverwritesManifestAndContainerfile(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "basepod.yaml", "name: old-name\nport: 1234\n")
	writeFile(t, dir, "Containerfile", "FROM scratch\n# old, hand-written\n")
	writeFile(t, dir, "go.mod", "module example.com/x\n\ngo 1.26\n")

	var out bytes.Buffer
	if err := runInit(&out, dir, true, 0, "new-name"); err != nil {
		t.Fatalf("runInit: %v", err)
	}

	manifest, err := os.ReadFile(filepath.Join(dir, "basepod.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(manifest), "name: new-name") {
		t.Fatalf("basepod.yaml = %q, want the overwritten name", manifest)
	}

	cf, err := os.ReadFile(filepath.Join(dir, "Containerfile"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(cf), "hand-written") {
		t.Fatalf("Containerfile = %q, want it overwritten by --force", cf)
	}
	if !strings.Contains(string(cf), "docker.io/library/golang") {
		t.Fatalf("Containerfile = %q, want the Go starter template", cf)
	}
}

// TestInitLeavesExistingContainerfileAloneWithoutForce proves a
// pre-existing Containerfile is left untouched (not an error) when no
// basepod.yaml exists yet — "starter Containerfile ONLY if none exists".
func TestInitLeavesExistingContainerfileAloneWithoutForce(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "Containerfile", "FROM scratch\n# hand-written, do not touch\n")
	writeFile(t, dir, "package.json", `{"name":"x"}`)

	var out bytes.Buffer
	if err := runInit(&out, dir, false, 0, ""); err != nil {
		t.Fatalf("runInit: %v", err)
	}

	cf, err := os.ReadFile(filepath.Join(dir, "Containerfile"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(cf), "hand-written, do not touch") {
		t.Fatalf("Containerfile = %q, want it left untouched", cf)
	}
	// basepod.yaml should still have been written, since it didn't exist.
	if _, err := os.Stat(filepath.Join(dir, "basepod.yaml")); err != nil {
		t.Fatalf("basepod.yaml was not written: %v", err)
	}
	if !strings.Contains(out.String(), "leaving it alone") {
		t.Errorf("output = %q, want a note that the Containerfile was left alone", out.String())
	}
}

// TestInitBadPortFlagIsRejected proves an out-of-range --port fails
// before anything is written.
func TestInitBadPortFlagIsRejected(t *testing.T) {
	dir := t.TempDir()
	var out bytes.Buffer
	err := runInit(&out, dir, false, 70000, "")
	if err == nil || !strings.Contains(err.Error(), "--port") {
		t.Fatalf("err = %v, want a --port range error", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "basepod.yaml")); statErr == nil {
		t.Fatal("basepod.yaml was written despite the invalid --port")
	}
}

// TestInitCommandRegistered is a thin smoke test proving `basepod init`
// is wired into the cobra command tree.
func TestInitCommandRegistered(t *testing.T) {
	dir := t.TempDir()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(cwd)

	out, _, err := runCLI(t, "", "init")
	if err != nil {
		t.Fatalf("init: %v (out=%s)", err, out)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "basepod.yaml")); statErr != nil {
		t.Fatalf("basepod.yaml not written: %v", statErr)
	}
}
