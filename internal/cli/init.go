package cli

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/base-al/basepod/internal/manifest"
)

// stack identifies a detected project type for `basepod init`'s
// Containerfile/basepod.yaml scaffolding.
type stack string

const (
	stackNode    stack = "node"
	stackGo      stack = "go"
	stackPython  stack = "python"
	stackStatic  stack = "static"
	stackGeneric stack = "generic"
)

// newInitCmd builds `basepod init`: scaffold a basepod.yaml (and, if
// none exists, a starter Containerfile) for the current directory, so
// `basepod deploy` works with zero prior dashboard setup.
func newInitCmd() *cobra.Command {
	var force bool
	var portFlag int
	var nameFlag string
	cmd := &cobra.Command{
		Use:          "init",
		Short:        "Scaffold a basepod.yaml (and a starter Containerfile) for zero-config deploys",
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			dir, err := os.Getwd()
			if err != nil {
				return fmt.Errorf("determine working directory: %w", err)
			}
			return runInit(cmd.OutOrStdout(), dir, force, portFlag, nameFlag)
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "overwrite an existing basepod.yaml and/or Containerfile")
	cmd.Flags().IntVar(&portFlag, "port", 0, "container port to expose (default: a sensible value for the detected stack)")
	cmd.Flags().StringVar(&nameFlag, "name", "", "app name (default: the current directory name, slugified)")
	return cmd
}

// detectStack inspects dir's top level for the marker files that decide
// which starter Containerfile/basepod.yaml `basepod init` writes. Order
// matters: a directory can have both a package.json AND an index.html
// (a frontend app with its own build tooling) — package.json wins,
// since building it through npm is the correct path, not serving the
// repo root as-is via a static file server.
func detectStack(dir string) stack {
	exists := func(name string) bool {
		info, err := os.Stat(filepath.Join(dir, name))
		return err == nil && info.Mode().IsRegular()
	}
	existsDir := func(name string) bool {
		info, err := os.Stat(filepath.Join(dir, name))
		return err == nil && info.IsDir()
	}
	switch {
	case exists("package.json"):
		return stackNode
	case exists("go.mod"):
		return stackGo
	case exists("requirements.txt"), exists("pyproject.toml"):
		return stackPython
	case exists("index.html"), existsDir("public"):
		return stackStatic
	default:
		return stackGeneric
	}
}

// defaultPort returns each stack's conventional container port —
// basepod.yaml's `port` and the starter Containerfile's EXPOSE agree on
// this so a fresh `basepod init && basepod deploy` just works.
func defaultPort(s stack) int {
	switch s {
	case stackNode:
		return 3000
	case stackGo:
		return 8080
	case stackPython:
		return 8000
	case stackStatic:
		return 80
	default:
		return 8080
	}
}

// slugifyName derives a valid app slug from name, matching the server's
// slug rules — see manifest.NamePattern, kept in sync by hand with
// internal/api's unexported slugPattern (`^[a-z][a-z0-9-]{0,31}$`).
// Unlike internal/api's own (deliberately minimal) slugify, which only
// lowercases and turns spaces into hyphens, this one collapses ANY run
// of non [a-z0-9-] characters into a single hyphen — a real directory
// name is far more likely to contain underscores, dots, or punctuation
// (e.g. "my_app.v2") than a user-typed app name ever was, and a name
// `init` can't turn into something the server will accept isn't a
// useful default.
func slugifyName(name string) string {
	lower := strings.ToLower(name)
	var b strings.Builder
	lastHyphen := true // suppresses a leading hyphen
	for _, r := range lower {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			lastHyphen = false
		default:
			if !lastHyphen {
				b.WriteByte('-')
				lastHyphen = true
			}
		}
	}
	s := strings.TrimRight(b.String(), "-")
	if s == "" || s[0] < 'a' || s[0] > 'z' {
		s = "app-" + s
	}
	if len(s) > 32 {
		s = s[:32]
	}
	s = strings.TrimRight(s, "-")
	if s == "" {
		s = "app"
	}
	return s
}

// existingManifestPath reports the path and filename of dir's
// basepod.yaml or basepod.yml, if either already exists (basepod.yaml
// checked first — matching the same preference build.validateTar uses).
func existingManifestPath(dir string) (path, name string) {
	for _, n := range []string{"basepod.yaml", "basepod.yml"} {
		p := filepath.Join(dir, n)
		if info, err := os.Stat(p); err == nil && info.Mode().IsRegular() {
			return p, n
		}
	}
	return "", ""
}

// existingContainerfilePath reports the path of dir's Containerfile or
// Dockerfile, if either already exists.
func existingContainerfilePath(dir string) (path string, exists bool) {
	for _, n := range []string{"Containerfile", "Dockerfile"} {
		p := filepath.Join(dir, n)
		if info, err := os.Stat(p); err == nil && info.Mode().IsRegular() {
			return p, true
		}
	}
	return "", false
}

// runInit implements `basepod init` against dir, writing to out. It's
// factored out from newInitCmd's RunE (which only resolves os.Getwd())
// so tests can point it at a t.TempDir() directly rather than needing to
// os.Chdir the whole test process.
func runInit(out io.Writer, dir string, force bool, portFlag int, nameFlag string) error {
	st := detectStack(dir)

	name := nameFlag
	if name == "" {
		name = filepath.Base(dir)
	}
	name = slugifyName(name)
	if !manifest.NamePattern.MatchString(name) {
		return fmt.Errorf("could not derive a valid app name from %q; pass --name explicitly", name)
	}

	port := portFlag
	if port == 0 {
		port = defaultPort(st)
	}
	if port < 1 || port > 65535 {
		return fmt.Errorf("--port must be between 1 and 65535, got %d", port)
	}

	manifestPath, existingManifestName := existingManifestPath(dir)
	if existingManifestName != "" && !force {
		return fmt.Errorf("%s already exists (use --force to overwrite)", filepath.Join(dir, existingManifestName))
	}
	if manifestPath == "" {
		manifestPath = filepath.Join(dir, "basepod.yaml")
	}
	if err := os.WriteFile(manifestPath, []byte(renderManifest(name, port)), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", manifestPath, err)
	}
	fmt.Fprintf(out, "wrote %s\n", manifestPath)

	cfPath, cfExists := existingContainerfilePath(dir)
	if cfExists && !force {
		fmt.Fprintf(out, "%s already exists, leaving it alone\n", cfPath)
	} else {
		if cfPath == "" {
			cfPath = filepath.Join(dir, "Containerfile")
		}
		content := renderContainerfile(dir, st, port)
		if err := os.WriteFile(cfPath, []byte(content), 0o644); err != nil {
			return fmt.Errorf("write %s: %w", cfPath, err)
		}
		fmt.Fprintf(out, "wrote %s\n", cfPath)
	}

	fmt.Fprintf(out, "\nDetected stack: %s\n", st)
	fmt.Fprintln(out, "Next steps:")
	fmt.Fprintln(out, "  basepod deploy")
	return nil
}

// renderManifest is the basepod.yaml content `basepod init` writes: name
// and port are set, everything else is left commented out as a
// reference — matching docs/plan/06.builds-and-deployments.md's example
// — since a zero-config deploy needs neither a healthcheck path nor
// build args nor resource limits to work at all.
func renderManifest(name string, port int) string {
	return fmt.Sprintf(`# basepod.yaml — generated by "basepod init".
# All fields are optional; flags passed to "basepod deploy" override
# these. See docs/plan/06.builds-and-deployments.md for the full
# reference.

name: %s
port: %d

# healthcheck:
#   path: /healthz

# build:
#   containerfile: ./Containerfile
#   args:
#     NODE_ENV: production

# env:
#   TZ: UTC

# resources:
#   memory: 512m
#   cpus: 1.0
`, name, port)
}

// renderContainerfile returns the starter Containerfile content for st.
// Every template is meant to work as-is for a conventional project of
// that stack — "honest and minimal" per the task brief, not a
// framework-detecting wizard — with comments pointing at the most common
// adjustment (a build step, a different entry point, etc.). Base images
// are pinned to concrete, verified-to-exist tags rather than "latest",
// so a fresh `basepod init` doesn't drift underneath a user.
func renderContainerfile(dir string, st stack, port int) string {
	switch st {
	case stackNode:
		return fmt.Sprintf(`# Starter Containerfile for a Node.js app — generated by "basepod init".
# Assumes "npm start" runs your app and it listens on $PORT (%d below,
# matching basepod.yaml's "port"). If your app has a separate build step
# (TypeScript, a bundler, a frontend framework), add it after "COPY . .".

FROM docker.io/library/node:22-alpine
WORKDIR /app
COPY package*.json ./
RUN npm ci
COPY . .
# RUN npm run build
EXPOSE %d
CMD ["npm", "start"]
`, port, port)

	case stackGo:
		return fmt.Sprintf(`# Starter Containerfile for a Go app — generated by "basepod init".
# Multi-stage: the build stage compiles a static binary, and the final
# image only carries that binary and CA certificates — much smaller than
# shipping the whole Go toolchain. Assumes "go build ." at the repo root
# produces your app's binary; adjust the build target if your main
# package lives elsewhere (e.g. "./cmd/server").

FROM docker.io/library/golang:1.26-alpine AS build
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 go build -o /out/app .

FROM docker.io/library/alpine:3.22
RUN apk add --no-cache ca-certificates
WORKDIR /app
COPY --from=build /out/app ./app
EXPOSE %d
CMD ["./app"]
`, port)

	case stackPython:
		if _, err := os.Stat(filepath.Join(dir, "requirements.txt")); err == nil {
			return fmt.Sprintf(`# Starter Containerfile for a Python app — generated by "basepod init".
# Assumes requirements.txt lists your dependencies and "python app.py"
# runs your app on $PORT (%d below, matching basepod.yaml's "port").
# Adjust CMD for your framework (e.g. gunicorn/uvicorn for a web app).

FROM docker.io/library/python:3.13-alpine
WORKDIR /app
COPY requirements.txt ./
RUN pip install --no-cache-dir -r requirements.txt
COPY . .
EXPOSE %d
CMD ["python", "app.py"]
`, port, port)
		}
		return fmt.Sprintf(`# Starter Containerfile for a Python app — generated by "basepod init".
# Assumes pyproject.toml declares your project (pip installs it directly)
# and "python app.py" runs your app on $PORT (%d below, matching
# basepod.yaml's "port"). Adjust CMD for your framework (e.g.
# gunicorn/uvicorn for a web app).

FROM docker.io/library/python:3.13-alpine
WORKDIR /app
COPY . .
RUN pip install --no-cache-dir .
EXPOSE %d
CMD ["python", "app.py"]
`, port, port)

	case stackStatic:
		return `# Starter Containerfile for a static site — generated by "basepod init".
# Serves the current directory's files as-is via Caddy's default
# Caddyfile (root /usr/share/caddy, file_server enabled) — no build step
# assumed. If you add one later (a bundler, a static site generator),
# switch this to a multi-stage build: build in an earlier stage, then
# COPY --from=build the output directory into this same final stage.

FROM docker.io/library/caddy:2.10-alpine
COPY . /usr/share/caddy
EXPOSE 80
`

	default: // stackGeneric
		return `# "basepod init" couldn't detect your project's stack (no package.json,
# go.mod, requirements.txt/pyproject.toml, or index.html/public/ found
# at the root of this directory). Replace this file with a real
# Containerfile for your app. A minimal shape looks like:
#
#   FROM <a base image for your runtime>
#   WORKDIR /app
#   COPY . .
#   RUN <install dependencies / build>
#   EXPOSE <port your app listens on>
#   CMD ["<command to run your app>"]
#
# This placeholder intentionally fails to build until you edit it —
# guessing a runtime here would be worse than asking you to pick one.
FROM docker.io/library/alpine:3.22
RUN echo "error: edit this Containerfile for your app's stack before deploying" >&2 && exit 1
`
	}
}
