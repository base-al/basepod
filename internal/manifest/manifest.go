// Package manifest parses and validates basepod.yaml — the optional,
// zero-config app manifest documented in
// docs/plan/06.builds-and-deployments.md. It has exactly one job: turn a
// YAML document into a *Manifest plus a list of non-fatal warnings, or a
// clear, field-named error for anything that can't be sensibly defaulted
// away. It has no dependency on internal/build, internal/deploy, or
// internal/cli — those packages depend on it, not the other way around.
package manifest

import (
	"fmt"
	"io"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// NamePattern is the DNS-label-safe shape a manifest's "name" must have.
// It is kept in sync BY HAND with internal/api's unexported slugPattern
// (`^[a-z][a-z0-9-]{0,31}$`): a deployed app's slug becomes part of a
// hostname (slug + "." + rootDomain), so it must start with a lowercase
// letter and contain only lowercase letters, digits, and hyphens, up to
// 32 characters total. internal/manifest intentionally does not import
// internal/api (nor the reverse) to avoid a dependency between the build
// pipeline and the HTTP API package, so this is a deliberate, documented
// duplication rather than a shared import — if the server's rule ever
// changes, this must change with it.
var NamePattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)

// Manifest is the v1 subset of basepod.yaml that the build pipeline and
// `basepod init` understand. Every field is optional — a zero value
// means "not set in the manifest," so a caller applies it only where the
// app doesn't already have its own setting (see internal/build.Result's
// doc comment for how the build pipeline threads this through).
type Manifest struct {
	// Name defaults an app's slug; "" means unset.
	Name string
	// Port is the container port to expose; 0 means unset.
	Port        int
	Healthcheck Healthcheck
	Build       Build
	// Env holds non-secret default environment variables. Secrets are
	// never set here — they're configured via the UI/CLI's `env` command
	// against the server's encrypted store.
	Env       map[string]string
	Resources Resources
}

// Healthcheck is the manifest's `healthcheck:` block.
type Healthcheck struct {
	// Path is the HTTP path to probe; "" means unset (server default is
	// a bare TCP connect / "/", per doc 06).
	Path string
}

// Build is the manifest's `build:` block.
type Build struct {
	// Containerfile overrides the auto-detected Containerfile/Dockerfile
	// path; "" means unset (auto-detect).
	Containerfile string
	// Args are passed to the container build as --build-arg.
	Args map[string]string
}

// Resources is the manifest's `resources:` block.
type Resources struct {
	// Memory is the raw string as written in the manifest (e.g. "512m",
	// "1g", or a plain byte count) — validated at parse time via
	// ParseMemory but kept as a string here, matching the unit
	// basepod.yaml is documented in. A caller wanting a byte count calls
	// ParseMemory(m.Resources.Memory) itself. "" means unset.
	Memory string
	// CPUs is the CPU core allotment (e.g. 1.5); 0 means unset.
	CPUs float64
}

// Parse reads and validates a basepod.yaml/basepod.yml document from r.
//
// It is strict-ish: keys it doesn't recognize — at the top level, or
// nested inside healthcheck/build/build.args/resources — are reported as
// warnings, not parse failures. A typo or a newer field an older
// BasePod binary doesn't know about yet must never be the reason a
// zero-config deploy fails outright.
//
// Type errors (a field holding the wrong kind of YAML value — e.g. port
// given as a string) and out-of-range values (a bad port, an unparseable
// memory string, non-positive cpus, an invalid name) ARE fatal: there is
// no sane default to silently fall back to, and guessing would be worse
// than failing loudly with a message naming the exact field.
//
// An empty document (or one that's just `null`, comments, or whitespace)
// parses to a zero-value Manifest with no warnings and no error — every
// field is optional.
func Parse(r io.Reader) (*Manifest, []string, error) {
	var raw map[string]interface{}
	if err := yaml.NewDecoder(r).Decode(&raw); err != nil {
		if err == io.EOF {
			return &Manifest{}, nil, nil
		}
		return nil, nil, fmt.Errorf("manifest: invalid yaml: %w", err)
	}
	if raw == nil {
		return &Manifest{}, nil, nil
	}

	m := &Manifest{}
	var warnings []string

	for key, val := range raw {
		switch key {
		case "name":
			s, ok := val.(string)
			if !ok {
				return nil, nil, typeErr("name", "a string", val)
			}
			if s != "" && !NamePattern.MatchString(s) {
				return nil, nil, fmt.Errorf("manifest: field 'name' must match %s (lowercase letters, digits, and hyphens, starting with a letter, max 32 chars), got %q", NamePattern.String(), s)
			}
			m.Name = s

		case "port":
			n, ok := toInt(val)
			if !ok {
				return nil, nil, typeErr("port", "an integer", val)
			}
			if n < 1 || n > 65535 {
				return nil, nil, fmt.Errorf("manifest: field 'port' must be between 1 and 65535, got %d", n)
			}
			m.Port = n

		case "healthcheck":
			hc, w, err := parseHealthcheck(val)
			if err != nil {
				return nil, nil, err
			}
			m.Healthcheck = hc
			warnings = append(warnings, w...)

		case "build":
			b, w, err := parseBuild(val)
			if err != nil {
				return nil, nil, err
			}
			m.Build = b
			warnings = append(warnings, w...)

		case "env":
			e, err := parseStringMap("env", val)
			if err != nil {
				return nil, nil, err
			}
			m.Env = e

		case "resources":
			res, w, err := parseResources(val)
			if err != nil {
				return nil, nil, err
			}
			m.Resources = res
			warnings = append(warnings, w...)

		default:
			warnings = append(warnings, fmt.Sprintf("unknown field: %s", key))
		}
	}

	sort.Strings(warnings)
	return m, warnings, nil
}

func parseHealthcheck(val interface{}) (Healthcheck, []string, error) {
	hcMap, ok := val.(map[string]interface{})
	if !ok {
		return Healthcheck{}, nil, typeErr("healthcheck", "a mapping", val)
	}
	var hc Healthcheck
	var warnings []string
	for k, v := range hcMap {
		switch k {
		case "path":
			s, ok := v.(string)
			if !ok {
				return Healthcheck{}, nil, typeErr("healthcheck.path", "a string", v)
			}
			hc.Path = s
		default:
			warnings = append(warnings, fmt.Sprintf("unknown field: healthcheck.%s", k))
		}
	}
	return hc, warnings, nil
}

func parseBuild(val interface{}) (Build, []string, error) {
	bMap, ok := val.(map[string]interface{})
	if !ok {
		return Build{}, nil, typeErr("build", "a mapping", val)
	}
	var b Build
	var warnings []string
	for k, v := range bMap {
		switch k {
		case "containerfile":
			s, ok := v.(string)
			if !ok {
				return Build{}, nil, typeErr("build.containerfile", "a string", v)
			}
			b.Containerfile = s
		case "args":
			args, err := parseStringMap("build.args", v)
			if err != nil {
				return Build{}, nil, err
			}
			b.Args = args
		default:
			warnings = append(warnings, fmt.Sprintf("unknown field: build.%s", k))
		}
	}
	return b, warnings, nil
}

func parseResources(val interface{}) (Resources, []string, error) {
	rMap, ok := val.(map[string]interface{})
	if !ok {
		return Resources{}, nil, typeErr("resources", "a mapping", val)
	}
	var res Resources
	var warnings []string
	for k, v := range rMap {
		switch k {
		case "memory":
			var s string
			if str, ok := v.(string); ok {
				s = str
			} else if n, ok := toInt(v); ok {
				// A bare integer (e.g. `memory: 536870912`) decodes as a
				// YAML int rather than a string — accept it as a plain
				// byte count rather than forcing users to quote it.
				s = strconv.Itoa(n)
			} else {
				return Resources{}, nil, typeErr("resources.memory", `a string (e.g. "512m")`, v)
			}
			if _, err := ParseMemory(s); err != nil {
				return Resources{}, nil, fmt.Errorf("manifest: field 'resources.memory': %w", err)
			}
			res.Memory = s
		case "cpus":
			f, ok := toFloat(v)
			if !ok {
				return Resources{}, nil, typeErr("resources.cpus", "a number", v)
			}
			if f <= 0 {
				return Resources{}, nil, fmt.Errorf("manifest: field 'resources.cpus' must be greater than 0, got %v", f)
			}
			res.CPUs = f
		default:
			warnings = append(warnings, fmt.Sprintf("unknown field: resources.%s", k))
		}
	}
	return res, warnings, nil
}

// parseStringMap validates val is a YAML mapping whose every value is a
// string (used for both `env` and `build.args`), returning a
// field-named error — e.g. "manifest: field 'env.PORT' must be a
// string, got an integer" — the moment it isn't.
func parseStringMap(field string, val interface{}) (map[string]string, error) {
	raw, ok := val.(map[string]interface{})
	if !ok {
		return nil, typeErr(field, "a mapping of strings", val)
	}
	out := make(map[string]string, len(raw))
	for k, v := range raw {
		s, ok := v.(string)
		if !ok {
			return nil, typeErr(field+"."+k, "a string", v)
		}
		out[k] = s
	}
	return out, nil
}

// memoryPattern matches an optional decimal number followed by an
// optional unit suffix — k/m/g (kibi/mebi/gibi-byte multiples, matching
// Docker's --memory convention), case-insensitive, with an optional
// trailing "b" so "512m", "512mb", and "512M" all work. No suffix means
// plain bytes.
var memoryPattern = regexp.MustCompile(`(?i)^([0-9]+(?:\.[0-9]+)?)\s*([kmg]?)b?$`)

// ParseMemory parses a basepod.yaml resources.memory value — "512m",
// "1g", or a plain byte count like "536870912" — into a byte count.
func ParseMemory(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty memory value")
	}
	match := memoryPattern.FindStringSubmatch(s)
	if match == nil {
		return 0, fmt.Errorf(`invalid memory value %q (expected e.g. "512m", "1g", or a plain byte count)`, s)
	}
	n, err := strconv.ParseFloat(match[1], 64)
	if err != nil {
		return 0, fmt.Errorf("invalid memory value %q: %w", s, err)
	}
	mult := 1.0
	switch strings.ToLower(match[2]) {
	case "k":
		mult = 1 << 10
	case "m":
		mult = 1 << 20
	case "g":
		mult = 1 << 30
	}
	bytesVal := n * mult
	if bytesVal <= 0 {
		return 0, fmt.Errorf("memory value %q must be greater than 0", s)
	}
	return int64(bytesVal), nil
}

// typeErr builds the "manifest: field 'X' must be Y, got Z" error every
// type mismatch reports, naming both the offending field and what it
// actually held so a user doesn't have to guess.
func typeErr(field, want string, got interface{}) error {
	return fmt.Errorf("manifest: field %q must be %s, got %s", field, want, yamlTypeName(got))
}

// yamlTypeName describes v the way a manifest author would recognize it
// from their own YAML, not as a Go type name.
func yamlTypeName(v interface{}) string {
	switch v.(type) {
	case nil:
		return "null"
	case string:
		return "a string"
	case bool:
		return "a boolean"
	case int, int64, uint64:
		return "an integer"
	case float64:
		return "a number"
	case map[string]interface{}:
		return "a mapping"
	case []interface{}:
		return "a list"
	default:
		return fmt.Sprintf("%T", v)
	}
}

// toInt reports v's integer value and whether v was one of the integer
// kinds yaml.v3 decodes a YAML scalar into (int for small values, int64
// or uint64 for larger ones) — never a float, so `port: 8080.0` (an
// explicit decimal point in the YAML) is correctly rejected as not an
// integer rather than silently truncated.
func toInt(v interface{}) (int, bool) {
	switch t := v.(type) {
	case int:
		return t, true
	case int64:
		return int(t), true
	case uint64:
		if t > uint64(math.MaxInt) {
			return 0, false
		}
		return int(t), true
	default:
		return 0, false
	}
}

// toFloat reports v's float64 value and whether v was any numeric kind
// (int, int64, uint64, or float64) — used for `resources.cpus`, which
// unlike port is meant to accept fractional values like 1.5.
func toFloat(v interface{}) (float64, bool) {
	switch t := v.(type) {
	case float64:
		return t, true
	case int:
		return float64(t), true
	case int64:
		return float64(t), true
	case uint64:
		return float64(t), true
	default:
		return 0, false
	}
}
