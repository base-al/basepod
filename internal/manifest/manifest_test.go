package manifest

import (
	"strings"
	"testing"
)

func TestParseValid(t *testing.T) {
	doc := `
name: blog
port: 8080
healthcheck:
  path: /healthz
build:
  containerfile: ./Containerfile
  args:
    NODE_ENV: production
env:
  TZ: Europe/Tirane
resources:
  memory: 512m
  cpus: 1.5
`
	m, warnings, err := Parse(strings.NewReader(doc))
	if err != nil {
		t.Fatalf("Parse: unexpected error: %v", err)
	}
	if len(warnings) != 0 {
		t.Fatalf("Parse: unexpected warnings: %v", warnings)
	}
	if m.Name != "blog" {
		t.Errorf("Name = %q, want blog", m.Name)
	}
	if m.Port != 8080 {
		t.Errorf("Port = %d, want 8080", m.Port)
	}
	if m.Healthcheck.Path != "/healthz" {
		t.Errorf("Healthcheck.Path = %q, want /healthz", m.Healthcheck.Path)
	}
	if m.Build.Containerfile != "./Containerfile" {
		t.Errorf("Build.Containerfile = %q, want ./Containerfile", m.Build.Containerfile)
	}
	if m.Build.Args["NODE_ENV"] != "production" {
		t.Errorf("Build.Args[NODE_ENV] = %q, want production", m.Build.Args["NODE_ENV"])
	}
	if m.Env["TZ"] != "Europe/Tirane" {
		t.Errorf("Env[TZ] = %q, want Europe/Tirane", m.Env["TZ"])
	}
	if m.Resources.Memory != "512m" {
		t.Errorf("Resources.Memory = %q, want 512m", m.Resources.Memory)
	}
	if m.Resources.CPUs != 1.5 {
		t.Errorf("Resources.CPUs = %v, want 1.5", m.Resources.CPUs)
	}
}

func TestParseEmptyDocument(t *testing.T) {
	for _, doc := range []string{"", "\n", "null\n", "# just a comment\n"} {
		m, warnings, err := Parse(strings.NewReader(doc))
		if err != nil {
			t.Fatalf("Parse(%q): unexpected error: %v", doc, err)
		}
		if len(warnings) != 0 {
			t.Fatalf("Parse(%q): unexpected warnings: %v", doc, warnings)
		}
		if m.Name != "" || m.Port != 0 || m.Healthcheck.Path != "" || m.Build.Containerfile != "" ||
			len(m.Build.Args) != 0 || len(m.Env) != 0 || m.Resources.Memory != "" || m.Resources.CPUs != 0 {
			t.Fatalf("Parse(%q): got non-zero manifest %+v", doc, m)
		}
	}
}

func TestParseUnknownKeysWarn(t *testing.T) {
	doc := `
name: blog
totally-unknown: yes
healthcheck:
  path: /healthz
  bogus: nested
resources:
  memory: 512m
  swap: 1g
`
	m, warnings, err := Parse(strings.NewReader(doc))
	if err != nil {
		t.Fatalf("Parse: unexpected error: %v", err)
	}
	want := []string{
		"unknown field: healthcheck.bogus",
		"unknown field: resources.swap",
		"unknown field: totally-unknown",
	}
	if !equalStrings(warnings, want) {
		t.Fatalf("warnings = %v, want %v", warnings, want)
	}
	// Unknown keys must not prevent the rest of the document from
	// parsing normally.
	if m.Name != "blog" || m.Healthcheck.Path != "/healthz" || m.Resources.Memory != "512m" {
		t.Fatalf("unexpected manifest after warnings: %+v", m)
	}
}

func TestParseTypeErrors(t *testing.T) {
	tests := []struct {
		name string
		doc  string
		want string // substring expected in the error
	}{
		{"port as string", "port: \"abc\"\n", `field "port" must be an integer`},
		{"name as int", "name: 123\n", `field "name" must be a string`},
		{"healthcheck as string", "healthcheck: nope\n", `field "healthcheck" must be a mapping`},
		{"healthcheck.path as int", "healthcheck:\n  path: 5\n", `field "healthcheck.path" must be a string`},
		{"build as string", "build: nope\n", `field "build" must be a mapping`},
		{"build.args value non-string", "build:\n  args:\n    FOO: 5\n", `field "build.args.FOO" must be a string`},
		{"env value non-string", "env:\n  FOO: 5\n", `field "env.FOO" must be a string`},
		{"resources.cpus as string", "resources:\n  cpus: \"a lot\"\n", `field "resources.cpus" must be a number`},
		{"resources.memory as bool", "resources:\n  memory: true\n", `field "resources.memory" must be`},
		{"malformed yaml", "name: [unterminated\n", "invalid yaml"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := Parse(strings.NewReader(tt.doc))
			if err == nil {
				t.Fatalf("Parse(%q): expected error, got nil", tt.doc)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Parse(%q): error = %q, want substring %q", tt.doc, err.Error(), tt.want)
			}
		})
	}
}

func TestParseBadPort(t *testing.T) {
	for _, doc := range []string{"port: 0\n", "port: -1\n", "port: 65536\n", "port: 999999\n"} {
		_, _, err := Parse(strings.NewReader(doc))
		if err == nil {
			t.Fatalf("Parse(%q): expected error, got nil", doc)
		}
		if !strings.Contains(err.Error(), "field 'port'") {
			t.Fatalf("Parse(%q): error = %q, want mention of field 'port'", doc, err.Error())
		}
	}
}

func TestParseBadName(t *testing.T) {
	tests := []string{
		"name: Blog\n",     // uppercase
		"name: -blog\n",    // starts with hyphen
		"name: 1blog\n",    // starts with digit
		"name: blog_app\n", // underscore not allowed
		"name: " + strings.Repeat("a", 33) + "\n", // too long
	}
	for _, doc := range tests {
		_, _, err := Parse(strings.NewReader(doc))
		if err == nil {
			t.Fatalf("Parse(%q): expected error, got nil", doc)
		}
		if !strings.Contains(err.Error(), "field 'name'") {
			t.Fatalf("Parse(%q): error = %q, want mention of field 'name'", doc, err.Error())
		}
	}
}

func TestParseBadMemory(t *testing.T) {
	for _, doc := range []string{
		"resources:\n  memory: nope\n",
		"resources:\n  memory: \"\"\n",
		"resources:\n  memory: -512m\n",
	} {
		_, _, err := Parse(strings.NewReader(doc))
		if err == nil {
			t.Fatalf("Parse(%q): expected error, got nil", doc)
		}
		if !strings.Contains(err.Error(), "resources.memory") {
			t.Fatalf("Parse(%q): error = %q, want mention of resources.memory", doc, err.Error())
		}
	}
}

func TestParseGoodMemoryUnits(t *testing.T) {
	tests := []struct {
		in   string
		want int64
	}{
		{"512m", 512 << 20},
		{"512M", 512 << 20},
		{"512mb", 512 << 20},
		{"1g", 1 << 30},
		{"1.5g", int64(1.5 * (1 << 30))},
		{"536870912", 536870912},
		{"1k", 1 << 10},
	}
	for _, tt := range tests {
		got, err := ParseMemory(tt.in)
		if err != nil {
			t.Fatalf("ParseMemory(%q): unexpected error: %v", tt.in, err)
		}
		if got != tt.want {
			t.Errorf("ParseMemory(%q) = %d, want %d", tt.in, got, tt.want)
		}
	}
}

func TestParseBadCPUs(t *testing.T) {
	for _, doc := range []string{
		"resources:\n  cpus: 0\n",
		"resources:\n  cpus: -1\n",
		"resources:\n  cpus: -0.5\n",
	} {
		_, _, err := Parse(strings.NewReader(doc))
		if err == nil {
			t.Fatalf("Parse(%q): expected error, got nil", doc)
		}
		if !strings.Contains(err.Error(), "resources.cpus") {
			t.Fatalf("Parse(%q): error = %q, want mention of resources.cpus", doc, err.Error())
		}
	}
}

func TestParseBareMemoryInteger(t *testing.T) {
	m, _, err := Parse(strings.NewReader("resources:\n  memory: 536870912\n"))
	if err != nil {
		t.Fatalf("Parse: unexpected error: %v", err)
	}
	if m.Resources.Memory != "536870912" {
		t.Fatalf("Resources.Memory = %q, want 536870912", m.Resources.Memory)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
