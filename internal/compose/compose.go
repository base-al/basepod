// Package compose parses the documented compose.yaml subset (see
// docs/plan/06.builds-and-deployments.md's "Compose support" section)
// and translates it into a plan of BasePod apps — one app per service,
// deployed in depends_on order on the shared network. It has exactly
// two jobs, split across two functions:
//
//   - Parse turns a compose.yaml document into a *File: everything the
//     documented subset can express, resolved as far as it can be
//     without knowing what project it belongs to or what else is
//     already deployed. Anything outside the subset is a warning
//     naming the offending key — never a silent drop — except bind
//     mounts, which are a hard error (a host path mount would escape
//     the container boundary onto the BasePod host).
//   - Plan takes a *File plus a project name and cross-project state
//     (PlanContext) and produces an ordered, validated *Plan of
//     per-service BasePod apps: slugs, network aliases, routing
//     (internal vs. public), a topological deploy order, and a
//     deploy-strategy recommendation for volume-bearing services.
//
// This package deliberately does NOT implement `podman kube play` —
// doc 06's ruling is direct translation to BasePod's own app model, so
// domains, env, and rollbacks keep working per-service instead of being
// bypassed by a generated Kubernetes manifest.
//
// This package has no dependency on internal/store, internal/api, or
// internal/deploy: Plan's inputs (PlanContext) and outputs (*Plan) are
// plain data so a caller (Task 8's compose-apply endpoint) can build
// PlanContext from the store and consume the Plan without this package
// reaching back into either.
package compose

import (
	"fmt"
	"io"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// slugPattern, reservedSlugPrefixPattern, and reservedSlugs are kept in
// sync BY HAND with internal/api's unexported equivalents of the same
// name — see internal/manifest.NamePattern's doc comment for why this
// package doesn't import internal/api (avoiding a dependency between
// the HTTP layer and the packages, like this one, that it depends on).
// If the server's slug rule ever changes, this must change with it.
var (
	slugPattern               = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)
	reservedSlugPrefixPattern = regexp.MustCompile(`^(bp-|app-)`)
	reservedSlugs             = map[string]bool{"caddy": true, "basepod": true}
)

// serviceNamePattern and the reserved* checks below apply the same
// shape and namespace guard to a compose *service* name itself, one
// level earlier than the derived slug: a service name becomes the bare
// network alias other services use to reach it (e.g. "db" from "web"),
// so it needs the same DNS-label safety and the same v0.4
// alias-namespace guard (audit finding M1 — see internal/deploy's
// runRollout and Routes()) as a slug does, independent of whatever the
// project-qualified slug ends up being.
var (
	serviceNamePattern    = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)
	reservedServicePrefix = regexp.MustCompile(`^(bp-|app-)`)
	reservedServiceNames  = map[string]bool{"caddy": true, "basepod": true}
)

// projectSlugPattern is the shape a normalized project name must have
// before it's combined with a service name into a full slug (see
// normalizeProjectSlug). It's deliberately unbounded in length here —
// the combined slug's 32-char cap is enforced once, in planService,
// where the actual limiting error message is produced.
var projectSlugPattern = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)

// File is the parsed, resolved representation of the documented
// compose.yaml subset. Parse rejects anything that can't be
// represented here (malformed YAML, bind mounts, a service with
// neither image nor build); everything else unsupported is a warning
// instead of a field. Map iteration in Go is randomized, so
// ServiceOrder — the order services appeared in the YAML document — is
// carried separately for deterministic downstream iteration (Plan's
// topological sort ties-break on it).
type File struct {
	// Project is the top-level `name:` field, "" if unset. Plan takes
	// its own project argument rather than defaulting to this
	// silently — a caller (Task 8) decides the precedence between a
	// query param and this field.
	Project string
	// Services holds every service, keyed by its compose name.
	Services map[string]*Service
	// ServiceOrder lists service names in YAML document order.
	ServiceOrder []string
	// Volumes holds the names declared under the top-level `volumes:`
	// key (empty if the key was absent).
	Volumes []string
	// Warnings holds file-scoped warnings only (unknown top-level
	// keys, a declared-but-unused top-level volume) — a service's own
	// warnings live on Service.Warnings instead. Parse's flat warning
	// list returned alongside *File is the sorted union of this field
	// and every service's Warnings.
	Warnings []string
}

// Service is one `services.<name>:` entry, fully resolved for
// everything in the documented subset.
type Service struct {
	Name        string
	Image       string
	Build       *BuildRef
	Environment map[string]string
	// Expose lists the container ports from `expose:`, in the order
	// given. Empty means the service has no expose entries (an
	// internal-only service) — Plan derives ServicePlan.Port and
	// .Internal from this list, and warns if it has more than one
	// entry (only the first routes).
	Expose    []int
	Volumes   []VolumeRef
	DependsOn []string
	// Warnings holds every warning this specific service produced
	// during Parse (unsupported keys, `ports` presence, extra expose
	// ports, depends_on long-form, unresolved-volume cross-checks).
	Warnings []string
}

// BuildRef is a resolved `build:` block, whether the compose file wrote
// it as a bare context string or the {context, dockerfile, args}
// mapping form. Shared verbatim between File (Parse's output) and
// ServicePlan (Plan's output) — Task 8 never needs to re-derive it.
type BuildRef struct {
	// Context defaults to "." when a mapping form omits it (matching
	// compose's own default); never empty.
	Context    string
	Dockerfile string
	Args       map[string]string
}

// VolumeRef is one named-volume mount, whether written in compose's
// short (`name:/path`) or long (`{type: volume, source, target}`)
// syntax. Bind mounts never reach this type — they're a Parse error.
// Shared verbatim between File and ServicePlan.
type VolumeRef struct {
	// Name is the named volume's name (not a host path — Parse
	// rejects those).
	Name string
	// Target is the container mount path.
	Target string
}

// Parse reads and validates a compose.yaml/compose.yml/docker-compose.yml
// document from r, translating the documented subset into a *File.
//
// Anything in the documented subset that's malformed (wrong YAML type,
// an invalid port, a service with neither image nor build) is a fatal,
// field-named error — there's no sane default to fall back to. A bind
// mount (a host path on either side of a volume mount) is also fatal:
// it isn't merely unsupported, it's a host-filesystem escape from the
// container boundary onto the BasePod host, so it's rejected rather
// than warned about. Everything else outside the subset — `ports`,
// `networks`, `restart`, `deploy`, `healthcheck`, an anonymous volume,
// depends_on's long condition form, an unknown top-level key, and so
// on — produces a warning naming the key and (where applicable) the
// service, and parsing continues: an unrecognized key from a newer
// compose feature must never be a silent drop, and it must never be
// the sole reason a file fails to translate.
//
// Malformed YAML (including a non-mapping top-level document) is a
// parse error pointing at the problem, never a panic.
//
// An empty document (or one that's just `null`, comments, or
// whitespace) parses to a *File with no services and no warnings.
func Parse(r io.Reader) (*File, []string, error) {
	var doc yaml.Node
	if err := yaml.NewDecoder(r).Decode(&doc); err != nil {
		if err == io.EOF {
			return emptyFile(), nil, nil
		}
		return nil, nil, fmt.Errorf("compose: invalid yaml: %w", err)
	}

	root := &doc
	if root.Kind == yaml.DocumentNode {
		if len(root.Content) == 0 {
			return emptyFile(), nil, nil
		}
		root = root.Content[0]
	}
	if isNullNode(root) {
		return emptyFile(), nil, nil
	}
	if root.Kind != yaml.MappingNode {
		return nil, nil, fmt.Errorf("compose: invalid yaml: top-level document must be a mapping, got %s", nodeKindName(root))
	}

	f := emptyFile()
	var fileWarnings []string
	declaredVolumes := map[string]bool{}
	haveVolumesSection := false
	var serviceKeys, serviceVals []*yaml.Node

	for i := 0; i+1 < len(root.Content); i += 2 {
		keyNode, valNode := root.Content[i], root.Content[i+1]
		switch keyNode.Value {
		case "name":
			var s string
			if err := valNode.Decode(&s); err != nil {
				return nil, nil, fmt.Errorf("compose: field 'name' must be a string, got %s", nodeKindName(valNode))
			}
			f.Project = s
		case "services":
			if valNode.Kind != yaml.MappingNode {
				return nil, nil, fmt.Errorf("compose: field 'services' must be a mapping, got %s", nodeKindName(valNode))
			}
			for j := 0; j+1 < len(valNode.Content); j += 2 {
				serviceKeys = append(serviceKeys, valNode.Content[j])
				serviceVals = append(serviceVals, valNode.Content[j+1])
			}
		case "volumes":
			var raw map[string]interface{}
			if err := valNode.Decode(&raw); err != nil {
				return nil, nil, fmt.Errorf("compose: field 'volumes' must be a mapping, got %s", nodeKindName(valNode))
			}
			haveVolumesSection = true
			for name := range raw {
				declaredVolumes[name] = true
				f.Volumes = append(f.Volumes, name)
			}
		case "version":
			// Legacy compose file-format marker. Ubiquitous in real
			// files, carries no translatable information, and
			// warning about it would just be noise — silently
			// accepted (the one deliberate exception to "every
			// unknown key warns").
		default:
			fileWarnings = append(fileWarnings, fmt.Sprintf("unknown top-level field: %s", keyNode.Value))
		}
	}
	sort.Strings(f.Volumes)

	usedVolumes := map[string][]string{} // volume name -> service names using it
	for i, keyNode := range serviceKeys {
		name := keyNode.Value
		if _, dup := f.Services[name]; dup {
			return nil, nil, fmt.Errorf("compose: duplicate service %q", name)
		}
		svc, err := parseService(name, serviceVals[i])
		if err != nil {
			return nil, nil, err
		}
		f.Services[name] = svc
		f.ServiceOrder = append(f.ServiceOrder, name)
		for _, vm := range svc.Volumes {
			usedVolumes[vm.Name] = append(usedVolumes[vm.Name], name)
		}
	}

	if haveVolumesSection {
		for volName, users := range usedVolumes {
			if declaredVolumes[volName] {
				continue
			}
			for _, u := range users {
				f.Services[u].Warnings = append(f.Services[u].Warnings,
					fmt.Sprintf("service %q: volume %q is not declared in top-level 'volumes:'", u, volName))
			}
		}
		for _, volName := range f.Volumes {
			if _, used := usedVolumes[volName]; !used {
				fileWarnings = append(fileWarnings, fmt.Sprintf("top-level volume %q is declared but not used by any service", volName))
			}
		}
	}

	sort.Strings(fileWarnings)
	f.Warnings = fileWarnings

	flat := append([]string(nil), fileWarnings...)
	for _, name := range f.ServiceOrder {
		sort.Strings(f.Services[name].Warnings)
		flat = append(flat, f.Services[name].Warnings...)
	}
	sort.Strings(flat)

	return f, flat, nil
}

func emptyFile() *File {
	return &File{Services: map[string]*Service{}}
}

func isNullNode(n *yaml.Node) bool {
	return n.Kind == yaml.ScalarNode && (n.Tag == "!!null" || n.Value == "")
}

func nodeKindName(n *yaml.Node) string {
	switch n.Kind {
	case yaml.MappingNode:
		return "a mapping"
	case yaml.SequenceNode:
		return "a list"
	case yaml.ScalarNode:
		return "a scalar"
	case yaml.AliasNode:
		return "an alias"
	default:
		return "an unknown node"
	}
}

// parseService resolves one services.<name>: entry. Field order within
// a service doesn't matter (unlike the top-level/services mapping, no
// yaml.Node bookkeeping is needed past this point — everything decodes
// into plain Go values, matching internal/manifest's style).
func parseService(name string, node *yaml.Node) (*Service, error) {
	if node.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("compose: service %q must be a mapping, got %s", name, nodeKindName(node))
	}
	var raw map[string]interface{}
	if err := node.Decode(&raw); err != nil {
		return nil, fmt.Errorf("compose: service %q: %w", name, err)
	}

	svc := &Service{Name: name}
	var warnings []string

	for key, val := range raw {
		switch key {
		case "image":
			s, ok := val.(string)
			if !ok {
				return nil, svcTypeErr(name, "image", "a string", val)
			}
			svc.Image = s

		case "build":
			b, w, err := parseBuildRef(name, val)
			if err != nil {
				return nil, err
			}
			svc.Build = b
			warnings = append(warnings, w...)

		case "environment":
			env, w, err := parseEnvironment(name, val)
			if err != nil {
				return nil, err
			}
			svc.Environment = env
			warnings = append(warnings, w...)

		case "volumes":
			vols, w, err := parseServiceVolumes(name, val)
			if err != nil {
				return nil, err
			}
			svc.Volumes = vols
			warnings = append(warnings, w...)

		case "depends_on":
			deps, w, err := parseDependsOn(name, val)
			if err != nil {
				return nil, err
			}
			svc.DependsOn = deps
			warnings = append(warnings, w...)

		case "expose":
			exp, w, err := parseExpose(name, val)
			if err != nil {
				return nil, err
			}
			svc.Expose = exp
			warnings = append(warnings, w...)

		case "ports":
			warnings = append(warnings, fmt.Sprintf(
				"service %q: key 'ports' (host port publishing) is not supported — routing happens via Caddy through 'expose' or a domain; ignored", name))

		default:
			warnings = append(warnings, fmt.Sprintf("service %q: unsupported key %q (ignored)", name, key))
		}
	}

	if svc.Image == "" && svc.Build == nil {
		return nil, fmt.Errorf("compose: service %q: must specify either 'image' or 'build'", name)
	}

	if svc.Environment == nil {
		svc.Environment = map[string]string{}
	}
	svc.Warnings = warnings
	return svc, nil
}

func parseBuildRef(service string, val interface{}) (*BuildRef, []string, error) {
	switch v := val.(type) {
	case string:
		return &BuildRef{Context: v}, nil, nil
	case map[string]interface{}:
		b := &BuildRef{}
		var warnings []string
		for k, vv := range v {
			switch k {
			case "context":
				s, ok := vv.(string)
				if !ok {
					return nil, nil, svcTypeErr(service, "build.context", "a string", vv)
				}
				b.Context = s
			case "dockerfile":
				s, ok := vv.(string)
				if !ok {
					return nil, nil, svcTypeErr(service, "build.dockerfile", "a string", vv)
				}
				b.Dockerfile = s
			case "args":
				switch av := vv.(type) {
				case map[string]interface{}:
					args, err := stringMap(service, "build.args", av)
					if err != nil {
						return nil, nil, err
					}
					b.Args = args
				case []interface{}:
					warnings = append(warnings, fmt.Sprintf(
						"service %q: key 'build.args' as a list is not supported; use a mapping — ignored", service))
				default:
					return nil, nil, svcTypeErr(service, "build.args", "a mapping", vv)
				}
			default:
				warnings = append(warnings, fmt.Sprintf("service %q: unsupported key %q (ignored)", service, "build."+k))
			}
		}
		if b.Context == "" {
			b.Context = "."
		}
		return b, warnings, nil
	default:
		return nil, nil, svcTypeErr(service, "build", "a string or a mapping", val)
	}
}

func stringMap(service, field string, raw map[string]interface{}) (map[string]string, error) {
	out := make(map[string]string, len(raw))
	for k, v := range raw {
		s, ok := v.(string)
		if !ok {
			return nil, svcTypeErr(service, field+"."+k, "a string", v)
		}
		out[k] = s
	}
	return out, nil
}

func parseEnvironment(service string, val interface{}) (map[string]string, []string, error) {
	switch v := val.(type) {
	case map[string]interface{}:
		out := map[string]string{}
		var warnings []string
		for k, vv := range v {
			if vv == nil {
				warnings = append(warnings, fmt.Sprintf(
					"service %q: environment variable %q has no value; treated as empty", service, k))
				out[k] = ""
				continue
			}
			s, ok := coerceScalar(vv)
			if !ok {
				return nil, nil, svcTypeErr(service, "environment."+k, "a string, number, or boolean", vv)
			}
			out[k] = s
		}
		return out, warnings, nil

	case []interface{}:
		out := map[string]string{}
		var warnings []string
		for _, item := range v {
			s, ok := item.(string)
			if !ok {
				return nil, nil, svcTypeErr(service, "environment[]", "a string (K=V)", item)
			}
			if idx := strings.Index(s, "="); idx >= 0 {
				out[s[:idx]] = s[idx+1:]
			} else {
				warnings = append(warnings, fmt.Sprintf(
					"service %q: environment entry %q has no value (host environment pass-through is not supported); treated as empty", service, s))
				out[s] = ""
			}
		}
		return out, warnings, nil

	default:
		return nil, nil, svcTypeErr(service, "environment", "a mapping or a list", val)
	}
}

func coerceScalar(v interface{}) (string, bool) {
	switch t := v.(type) {
	case string:
		return t, true
	case bool:
		return strconv.FormatBool(t), true
	case int:
		return strconv.Itoa(t), true
	case int64:
		return strconv.FormatInt(t, 10), true
	case uint64:
		return strconv.FormatUint(t, 10), true
	case float64:
		return strconv.FormatFloat(t, 'g', -1, 64), true
	default:
		return "", false
	}
}

func parseServiceVolumes(service string, val interface{}) ([]VolumeRef, []string, error) {
	list, ok := val.([]interface{})
	if !ok {
		return nil, nil, svcTypeErr(service, "volumes", "a list", val)
	}
	var out []VolumeRef
	var warnings []string
	for _, item := range list {
		switch v := item.(type) {
		case string:
			vol, warn, skip, err := parseShortVolume(service, v)
			if err != nil {
				return nil, nil, err
			}
			if warn != "" {
				warnings = append(warnings, warn)
			}
			if !skip {
				out = append(out, vol)
			}
		case map[string]interface{}:
			vol, warn, skip, err := parseLongVolume(service, v)
			if err != nil {
				return nil, nil, err
			}
			if warn != "" {
				warnings = append(warnings, warn)
			}
			if !skip {
				out = append(out, vol)
			}
		default:
			return nil, nil, svcTypeErr(service, "volumes[]", "a string or a mapping", item)
		}
	}
	return out, warnings, nil
}

// parseShortVolume resolves one `volumes:` list entry written in
// compose's short string syntax: "target" (anonymous, unsupported),
// "source:target", or "source:target:mode". A bind-mount source (a
// host path — absolute, or relative starting with "." or "~") is a
// hard error, never a warning: it would let a container reach outside
// its boundary onto the BasePod host's filesystem.
func parseShortVolume(service, spec string) (vol VolumeRef, warn string, skip bool, err error) {
	parts := strings.Split(spec, ":")
	switch len(parts) {
	case 1:
		return VolumeRef{}, fmt.Sprintf(
			"service %q: anonymous volume mount %q is not supported (named volumes only); skipped", service, spec), true, nil
	case 2, 3:
		source, target := parts[0], parts[1]
		if source == "" || target == "" {
			return VolumeRef{}, "", false, fmt.Errorf("compose: service %q: invalid volume mount %q", service, spec)
		}
		if isBindMountSource(source) {
			return VolumeRef{}, "", false, bindMountErr(service, spec)
		}
		if len(parts) == 3 && parts[2] != "" {
			warn = fmt.Sprintf("service %q: volume mount mode %q on %q is not enforced by BasePod", service, parts[2], spec)
		}
		return VolumeRef{Name: source, Target: target}, warn, false, nil
	default:
		return VolumeRef{}, "", false, fmt.Errorf("compose: service %q: invalid volume mount %q", service, spec)
	}
}

// parseLongVolume resolves one `volumes:` list entry written in
// compose's long mapping syntax ({type, source, target, ...}).
func parseLongVolume(service string, m map[string]interface{}) (vol VolumeRef, warn string, skip bool, err error) {
	typ, _ := m["type"].(string)
	if typ == "" {
		typ = "volume"
	}
	switch typ {
	case "bind":
		src, _ := m["source"].(string)
		return VolumeRef{}, "", false, bindMountErr(service, src)
	case "volume":
		source, _ := m["source"].(string)
		target, _ := m["target"].(string)
		if source == "" || target == "" {
			return VolumeRef{}, "", false, fmt.Errorf("compose: service %q: volume mount missing 'source' or 'target'", service)
		}
		if isBindMountSource(source) {
			return VolumeRef{}, "", false, bindMountErr(service, source)
		}
		return VolumeRef{Name: source, Target: target}, "", false, nil
	default:
		return VolumeRef{}, fmt.Sprintf(
			"service %q: volume type %q is not supported (named volumes only); skipped", service, typ), true, nil
	}
}

func bindMountErr(service, mount string) error {
	return fmt.Errorf(
		"compose: service %q: bind mount %q is not allowed — BasePod only supports named volumes (a host path mount would escape the container boundary onto the BasePod host)",
		service, mount)
}

// isBindMountSource reports whether a volume's short-syntax source
// looks like a host path rather than a named volume: absolute
// ("/data"), or relative starting with "." or "~" ("./data", "../x",
// "~/data"). A named volume's source is a bare name with none of
// those prefixes.
func isBindMountSource(s string) bool {
	if s == "" {
		return false
	}
	return strings.HasPrefix(s, "/") || strings.HasPrefix(s, "./") ||
		strings.HasPrefix(s, "../") || s == "." || s == ".." || strings.HasPrefix(s, "~")
}

func parseDependsOn(service string, val interface{}) ([]string, []string, error) {
	switch v := val.(type) {
	case []interface{}:
		var out []string
		for _, item := range v {
			s, ok := item.(string)
			if !ok {
				return nil, nil, svcTypeErr(service, "depends_on[]", "a string", item)
			}
			out = append(out, s)
		}
		return out, nil, nil

	case map[string]interface{}:
		var out []string
		for k := range v {
			out = append(out, k)
		}
		sort.Strings(out)
		warn := fmt.Sprintf(
			"service %q: depends_on's long form (condition) is accepted, but conditions are not enforced — only ordering is honored", service)
		return out, []string{warn}, nil

	default:
		return nil, nil, svcTypeErr(service, "depends_on", "a list or a mapping", val)
	}
}

func parseExpose(service string, val interface{}) ([]int, []string, error) {
	list, ok := val.([]interface{})
	if !ok {
		return nil, nil, svcTypeErr(service, "expose", "a list", val)
	}
	var out []int
	for _, item := range list {
		var s string
		switch t := item.(type) {
		case string:
			s = t
		case int:
			out = append(out, t)
			continue
		case int64:
			out = append(out, int(t))
			continue
		case uint64:
			out = append(out, int(t))
			continue
		default:
			return nil, nil, svcTypeErr(service, "expose[]", "a port number", item)
		}
		s = strings.SplitN(s, "/", 2)[0]
		n, convErr := strconv.Atoi(strings.TrimSpace(s))
		if convErr != nil {
			return nil, nil, fmt.Errorf("compose: service %q: expose entry %q is not a valid port number", service, item)
		}
		out = append(out, n)
	}
	for _, p := range out {
		if p < 1 || p > 65535 {
			return nil, nil, fmt.Errorf("compose: service %q: expose port %d is out of range (1-65535)", service, p)
		}
	}
	var warnings []string
	if len(out) > 1 {
		extras := make([]string, len(out)-1)
		for i, p := range out[1:] {
			extras[i] = strconv.Itoa(p)
		}
		warnings = append(warnings, fmt.Sprintf(
			"service %q: expose lists multiple ports (%s); only the first (%d) is used for routing — the rest are ignored",
			service, strings.Join(extras, ", "), out[0]))
	}
	return out, warnings, nil
}

func svcTypeErr(service, field, want string, got interface{}) error {
	return fmt.Errorf("compose: service %q: field %q must be %s, got %s", service, field, want, yamlTypeName(got))
}

// yamlTypeName describes v the way a compose.yaml author would
// recognize it, not as a Go type name. Deliberately duplicated from
// internal/manifest's identical helper rather than exported and
// shared — see this file's package doc comment on why this package
// has no dependency on any sibling package.
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

// ---- Plan ----

// ServiceKey identifies a compose-managed app by its project and
// service name — the same (project, service) pair Plan uses to derive
// slugs and, via PlanContext, decide create vs. update.
type ServiceKey struct {
	Project string
	Service string
}

// ExistingApp is what a caller (Task 8, backed by internal/store)
// reports about a (project, service) pair that a prior compose apply
// already created, so Plan can decide create vs. update, reuse the
// app's actual stored slug, and recognize that a service's own
// existing alias claim isn't a foreign collision against itself.
type ExistingApp struct {
	Slug  string
	Alias string
}

// PlanContext supplies Plan with the cross-project state a single
// compose.yaml can't see on its own: this project's apps from a prior
// apply (ExistingApps), and every network alias already claimed
// anywhere on the shared network (TakenAliases) — other projects'
// compose services and any hand-created app — so a bare service-name
// alias collision (audit finding M1's regression risk) is caught here,
// before Task 8 ever touches a container. The caller builds both maps
// from internal/store; this package never imports it.
type PlanContext struct {
	ExistingApps map[ServiceKey]ExistingApp
	// TakenAliases maps every claimed network alias to a
	// human-readable description of its owner, used verbatim in the
	// collision error (e.g. "app my-blog" or "compose project shop,
	// service db"). A service that already owns its own alias (an
	// update, not a create) should still be represented here — Plan
	// recognizes the self-owned case via ExistingApps, not by
	// inspecting this map's description text, so it's safe (and
	// expected) for a project's own prior aliases to appear in this
	// map too.
	TakenAliases map[string]string
}

// Plan is the fully validated, dependency-ordered translation of a
// compose *File into BasePod apps for one project. Services appear in
// Plan.Services in the order they must be deployed (topological order
// over depends_on) — Task 8's orchestrator deploys them sequentially in
// this order and stops the chain on the first failure.
type Plan struct {
	Project  string
	Services []ServicePlan
	// Warnings holds file-scoped warnings carried over from Parse
	// (unknown top-level keys, an unused top-level volume) — a
	// service's own warnings live on its ServicePlan.Warnings instead.
	Warnings []string
}

// ServicePlan is one service's fully resolved translation: everything
// Task 8 needs to create or update the app, apply its network alias,
// mount its volumes, and pick a deploy strategy — without reaching
// back into the original YAML.
type ServicePlan struct {
	Name, Slug, Image string
	Build             *BuildRef
	// Port is the container port to route to; 0 when Internal is true.
	Port int
	// Internal is true when the service had no `expose:` entries: no
	// Caddy route, no probe target — how a compose `db` stays private
	// on the shared network while still reachable by its Alias.
	Internal bool
	Env      map[string]string
	Volumes  []VolumeRef
	// DependsOn lists this service's own dependencies (informational —
	// Plan.Services is already in the order they imply; a caller
	// doesn't need to re-derive ordering from this field).
	DependsOn []string
	// Alias is the network alias Task 8 must apply to this service's
	// container, alongside its normal app-<slug> alias, so other
	// services can reach it by compose service name (e.g. "db"). It's
	// always equal to Name — surfaced explicitly here so Task 8 never
	// has to know that rule itself.
	Alias string
	// Action is "create" or "update": "update" when PlanContext
	// reported an existing app for this (project, service).
	Action string
	// RecommendedStrategy is "" (no change from the app's/store's
	// default) or "replace" — set whenever Volumes is non-empty, since
	// a zero-downtime overlap would give two containers write access
	// to the same volume (doc 06 / the Task 6 `replace` strategy this
	// plan exists to recommend). StrategyReason explains why in
	// prose; "" whenever RecommendedStrategy is "".
	RecommendedStrategy string
	StrategyReason      string
	// Warnings holds every warning that concerns this specific
	// service, carried over from Parse's Service.Warnings (this
	// package produces no additional per-service warnings of its own
	// at Plan time — every Plan-time problem is fatal, since it can
	// only be resolved by editing the compose file or the project
	// name, not silently worked around).
	Warnings []string
}

// BuildPlan translates f into a dependency-ordered, validated Plan for
// project, using existing to resolve create-vs-update and to detect
// network-alias collisions against everything else already deployed.
//
// (Named BuildPlan rather than "Plan" — the milestone plan's task
// brief sketches this as `compose.Plan(...) (*Plan, error)`, but a
// function and a type can't share one identifier in Go; the type is
// named Plan because it's the noun this whole file is about (see also
// ServicePlan, PlanContext), so the function took the verb form
// instead. Task 8 should call compose.BuildPlan.)
//
// Errors (never partial results): project doesn't normalize to a valid
// slug component; a service name isn't a valid DNS label or is
// reserved (must not be "caddy"/"basepod" and must not start with
// "bp-"/"app-" — the same guard slugs get, applied to the alias
// itself); the derived "<project>-<service>" slug is invalid, too
// long (>32 chars), or reserved; a service's bare-name network alias
// is already claimed by another project's service or an existing app;
// depends_on names an undefined service; or the depends_on graph has a
// cycle (the error names it, e.g. "a -> b -> a").
func BuildPlan(f *File, project string, existing PlanContext) (*Plan, error) {
	if f == nil {
		return nil, fmt.Errorf("compose: plan: file is required")
	}
	projSlug, err := normalizeProjectSlug(project)
	if err != nil {
		return nil, err
	}

	order, err := topoOrder(f)
	if err != nil {
		return nil, err
	}

	plan := &Plan{Project: project, Warnings: append([]string(nil), f.Warnings...)}
	for _, name := range order {
		sp, err := planService(projSlug, project, f.Services[name], existing)
		if err != nil {
			return nil, err
		}
		plan.Services = append(plan.Services, *sp)
	}
	return plan, nil
}

func normalizeProjectSlug(project string) (string, error) {
	if strings.TrimSpace(project) == "" {
		return "", fmt.Errorf("compose: plan: project name must not be empty")
	}
	norm := strings.ReplaceAll(strings.ToLower(project), " ", "-")
	if !projectSlugPattern.MatchString(norm) {
		return "", fmt.Errorf(
			"compose: plan: project name %q normalizes to %q, which is not a valid slug component (must start with a lowercase letter and contain only lowercase letters, digits, and hyphens)",
			project, norm)
	}
	return norm, nil
}

func planService(projSlug, project string, svc *Service, existing PlanContext) (*ServicePlan, error) {
	name := svc.Name

	if !serviceNamePattern.MatchString(name) {
		return nil, fmt.Errorf(
			"compose: plan: service %q: name must be a valid DNS label (lowercase letters, digits, and hyphens, starting with a letter, max 63 chars) to be used as a network alias", name)
	}
	if reservedServiceNames[name] || reservedServicePrefix.MatchString(name) {
		return nil, fmt.Errorf(
			"compose: plan: service %q: name is reserved — must not be \"caddy\" or \"basepod\", and must not start with \"bp-\" or \"app-\" (reserved for the managed proxy and the container/alias namespaces)", name)
	}

	slug := projSlug + "-" + name
	if len(slug) > 32 {
		return nil, fmt.Errorf(
			"compose: plan: service %q: derived slug %q is %d characters, exceeding the 32-character limit — shorten the project or service name",
			name, slug, len(slug))
	}
	if !slugPattern.MatchString(slug) {
		return nil, fmt.Errorf(
			"compose: plan: service %q: derived slug %q does not match the required pattern %s",
			name, slug, slugPattern.String())
	}
	if reservedSlugs[slug] || reservedSlugPrefixPattern.MatchString(slug) {
		return nil, fmt.Errorf(
			"compose: plan: service %q: derived slug %q is reserved — must not be \"caddy\" or \"basepod\", and must not start with \"bp-\" or \"app-\"",
			name, slug)
	}

	key := ServiceKey{Project: project, Service: name}
	existingApp, isUpdate := existing.ExistingApps[key]
	action := "create"
	if isUpdate {
		action = "update"
		if existingApp.Slug != "" {
			// Trust the store's record of this app's actual slug over
			// recomputing it — they should always agree, but the
			// stored value is the true identity Task 8 must target.
			slug = existingApp.Slug
		}
	}

	alias := name
	if owner, taken := existing.TakenAliases[alias]; taken {
		selfOwns := isUpdate && existingApp.Alias == alias
		if !selfOwns {
			return nil, fmt.Errorf(
				"compose: plan: service %q's network alias %q is already claimed by %s", name, alias, owner)
		}
	}

	port := 0
	internal := true
	if len(svc.Expose) > 0 {
		internal = false
		port = svc.Expose[0]
	}

	var recStrategy, recReason string
	if len(svc.Volumes) > 0 {
		names := make([]string, len(svc.Volumes))
		for i, v := range svc.Volumes {
			names[i] = v.Name
		}
		recStrategy = "replace"
		recReason = fmt.Sprintf(
			"service has named volume(s) %s — a zero-downtime deploy briefly runs the old and new container together, which would give two containers write access to the same volume and can corrupt data",
			strings.Join(names, ", "))
	}

	var buildRef *BuildRef
	if svc.Build != nil {
		b := *svc.Build
		buildRef = &b
	}

	return &ServicePlan{
		Name:                name,
		Slug:                slug,
		Image:               svc.Image,
		Build:               buildRef,
		Port:                port,
		Internal:            internal,
		Env:                 svc.Environment,
		Volumes:             append([]VolumeRef(nil), svc.Volumes...),
		DependsOn:           append([]string(nil), svc.DependsOn...),
		Alias:               alias,
		Action:              action,
		RecommendedStrategy: recStrategy,
		StrategyReason:      recReason,
		Warnings:            append([]string(nil), svc.Warnings...),
	}, nil
}

// topoOrder returns f's services in dependency order (a service always
// comes after everything in its own depends_on list), breaking ties
// deterministically by f.ServiceOrder. An error names either an
// undefined dependency or, if the graph can't be fully ordered, the
// cycle responsible.
func topoOrder(f *File) ([]string, error) {
	for _, name := range f.ServiceOrder {
		for _, dep := range f.Services[name].DependsOn {
			if _, ok := f.Services[dep]; !ok {
				return nil, fmt.Errorf("compose: plan: service %q depends_on undefined service %q", name, dep)
			}
		}
	}

	indegree := map[string]int{}
	dependents := map[string][]string{}
	for _, name := range f.ServiceOrder {
		indegree[name] = len(f.Services[name].DependsOn)
		for _, dep := range f.Services[name].DependsOn {
			dependents[dep] = append(dependents[dep], name)
		}
	}

	remaining := map[string]bool{}
	for _, name := range f.ServiceOrder {
		remaining[name] = true
	}

	var order []string
	for len(remaining) > 0 {
		picked := ""
		for _, name := range f.ServiceOrder {
			if remaining[name] && indegree[name] == 0 {
				picked = name
				break
			}
		}
		if picked == "" {
			return nil, fmt.Errorf("compose: plan: depends_on cycle: %s", strings.Join(findCycle(f), " -> "))
		}
		order = append(order, picked)
		delete(remaining, picked)
		for _, dependent := range dependents[picked] {
			indegree[dependent]--
		}
	}
	return order, nil
}

// findCycle locates one cycle in f's depends_on graph via a standard
// three-color DFS, for naming in topoOrder's error. Called only after
// topoOrder has already determined a cycle exists somewhere.
func findCycle(f *File) []string {
	const (
		white = iota
		gray
		black
	)
	color := map[string]int{}
	var path []string
	var result []string

	var dfs func(name string) bool
	dfs = func(name string) bool {
		color[name] = gray
		path = append(path, name)
		for _, dep := range f.Services[name].DependsOn {
			switch color[dep] {
			case gray:
				idx := 0
				for i, p := range path {
					if p == dep {
						idx = i
						break
					}
				}
				result = append(append([]string{}, path[idx:]...), dep)
				return true
			case white:
				if dfs(dep) {
					return true
				}
			}
		}
		path = path[:len(path)-1]
		color[name] = black
		return false
	}

	for _, name := range f.ServiceOrder {
		if color[name] == white {
			if dfs(name) {
				return result
			}
		}
	}
	return []string{"(unknown)"}
}
