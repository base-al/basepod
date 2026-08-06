package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/base-al/basepod/internal/manifest"
)

// sizeWarnThreshold is the packed (gzipped) build-context size past which
// `basepod deploy` prints a warning (but still proceeds) before uploading
// — a large upload isn't an error, just possibly a slow one, or a sign
// .basepodignore is missing an entry like a dependency or build-output
// directory.
const sizeWarnThreshold = 64 << 20 // 64 MiB

// Commands returns basepod's client-facing command tree — login, context,
// apps, deploy, logs, env, rollback, status — for cmd/basepod/main.go to
// add under its root command.
func Commands() []*cobra.Command {
	return []*cobra.Command{
		newLoginCmd(),
		newLogoutCmd(),
		newContextCmd(),
		newAppsCmd(),
		newInitCmd(),
		newDeployCmd(),
		newComposeCmd(),
		newLogsCmd(),
		newEnvCmd(),
		newRollbackCmd(),
		newStatusCmd(),
		newGitCmd(),
	}
}

// addContextFlag adds the --context flag every server-talking command
// accepts to override the config's current context for that one
// invocation, without changing what `basepod context use` leaves active.
func addContextFlag(cmd *cobra.Command) {
	cmd.Flags().String("context", "", "use this saved context instead of the current one")
}

// resolveClient loads the CLI config and builds a Client for the command's
// --context flag (falling back to the config's current context), or
// ErrNotLoggedIn if neither names a saved context.
func resolveClient(cmd *cobra.Command) (*Client, error) {
	cfg, err := LoadConfig()
	if err != nil {
		return nil, err
	}
	name, _ := cmd.Flags().GetString("context")
	ctxCfg, _, err := cfg.CurrentContext(name)
	if err != nil {
		return nil, err
	}
	return NewClient(ctxCfg.URL, ctxCfg.Token), nil
}

// readLine reads a single line from r, stripped of its trailing newline —
// shared by login's email/password prompts. Returns "" (no error) at EOF
// with no trailing newline, matching bufio.Reader.ReadString's own
// io.EOF-with-partial-data behavior.
func readLine(r *bufio.Reader) (string, error) {
	line, err := r.ReadString('\n')
	if err != nil && line == "" {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

// contextNameFromURL derives a default context name from a server URL —
// its host (e.g. "basepod.example.com" or "localhost:8080") — falling
// back to the raw URL string if it doesn't parse into one.
func contextNameFromURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return raw
	}
	return u.Host
}

// newLoginCmd builds `basepod login <url>`.
func newLoginCmd() *cobra.Command {
	var email, ctxName string
	cmd := &cobra.Command{
		Use:          "login <url>",
		Short:        "Log in to a BasePod server and save it as a context",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			serverURL := strings.TrimRight(args[0], "/")
			in := bufio.NewReader(cmd.InOrStdin())
			out := cmd.OutOrStdout()

			if email == "" {
				fmt.Fprint(out, "Email: ")
				line, err := readLine(in)
				if err != nil {
					return fmt.Errorf("read email: %w", err)
				}
				email = line
			}

			// Deliberately no --password flag (and no golang.org/x/term
			// dependency — see the task brief): a password flag would land
			// in shell history and be visible to anyone on the box via
			// `ps`, so the password is always read as a plain line from
			// stdin instead, echoed like any other terminal input. Warn so
			// that isn't a surprise over e.g. a screen-shared terminal.
			fmt.Fprintln(out, "Warning: password will be echoed (typed input is not masked)")
			fmt.Fprint(out, "Password: ")
			password, err := readLine(in)
			if err != nil {
				return fmt.Errorf("read password: %w", err)
			}

			client := NewClient(serverURL, "")
			result, err := client.Login(cmd.Context(), email, password)
			if err != nil {
				return err
			}

			name := ctxName
			if name == "" {
				name = contextNameFromURL(serverURL)
			}

			cfg, err := LoadConfig()
			if err != nil {
				return err
			}
			cfg.Contexts[name] = Context{URL: serverURL, Token: result.Token}
			cfg.Current = name
			if err := SaveConfig(cfg); err != nil {
				return err
			}

			fmt.Fprintf(out, "Logged in as %s (context %q)\n", result.User.Email, name)
			return nil
		},
	}
	cmd.Flags().StringVar(&email, "email", "", "account email (prompted if omitted)")
	cmd.Flags().StringVar(&ctxName, "context", "", "name for the saved context (default: the URL's host)")
	return cmd
}

// newLogoutCmd builds `basepod logout`: revokes the current context's
// session server-side (best-effort — see below) and then clears its saved
// token, leaving the context entry itself in place (so `basepod login`
// against the same server later reuses the same context name, and
// `basepod context list` still shows it). Scope is deliberately narrow:
// only the current (or --context-named) context's token is touched, never
// every saved context at once — a broader "log out everywhere" is out of
// scope for this command.
func newLogoutCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:          "logout",
		Short:        "Log out of the current context, revoking its session server-side",
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := LoadConfig()
			if err != nil {
				return err
			}
			name, _ := cmd.Flags().GetString("context")
			ctxCfg, resolvedName, err := cfg.CurrentContext(name)
			if err != nil {
				return err
			}

			// The server-side revoke is best-effort: a token that's already
			// expired or was already revoked (e.g. a double logout, or a
			// prior password change that killed it) must not block clearing
			// the now-useless local token — the whole point of this command
			// is to leave the CLI with no usable credential for this
			// context, and failing to reach the server is not a reason to
			// keep one lying around on disk.
			logoutErr := NewClient(ctxCfg.URL, ctxCfg.Token).Logout(cmd.Context())

			ctxCfg.Token = ""
			cfg.Contexts[resolvedName] = ctxCfg
			if err := SaveConfig(cfg); err != nil {
				return err
			}

			out := cmd.OutOrStdout()
			if logoutErr != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "warning: could not revoke session server-side: %v\n", logoutErr)
			}
			fmt.Fprintf(out, "Logged out of context %q\n", resolvedName)
			return nil
		},
	}
	addContextFlag(cmd)
	return cmd
}

// newContextCmd builds `basepod context list|use`.
func newContextCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "context",
		Short: "Manage saved server contexts",
	}
	cmd.AddCommand(newContextListCmd(), newContextUseCmd())
	return cmd
}

// contextListEntry is the --json shape of one `context list` row.
type contextListEntry struct {
	Name    string `json:"name"`
	URL     string `json:"url"`
	Current bool   `json:"current"`
}

func newContextListCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:          "list",
		Short:        "List saved contexts",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := LoadConfig()
			if err != nil {
				return err
			}
			names := make([]string, 0, len(cfg.Contexts))
			for n := range cfg.Contexts {
				names = append(names, n)
			}
			sort.Strings(names)

			if asJSON {
				entries := make([]contextListEntry, 0, len(names))
				for _, n := range names {
					entries = append(entries, contextListEntry{Name: n, URL: cfg.Contexts[n].URL, Current: n == cfg.Current})
				}
				return printJSON(cmd.OutOrStdout(), entries)
			}

			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 2, 2, ' ', 0)
			fmt.Fprintln(tw, "NAME\tURL\tCURRENT")
			for _, n := range names {
				current := ""
				if n == cfg.Current {
					current = "*"
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\n", n, cfg.Contexts[n].URL, current)
			}
			return tw.Flush()
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "output raw JSON instead of a table")
	return cmd
}

func newContextUseCmd() *cobra.Command {
	return &cobra.Command{
		Use:          "use <name>",
		Short:        "Set the current context",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := LoadConfig()
			if err != nil {
				return err
			}
			if _, ok := cfg.Contexts[args[0]]; !ok {
				return fmt.Errorf("context %q not found", args[0])
			}
			cfg.Current = args[0]
			if err := SaveConfig(cfg); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Switched to context %q\n", args[0])
			return nil
		},
	}
}

// newAppsCmd builds `basepod apps`.
func newAppsCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:          "apps",
		Short:        "List apps",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := resolveClient(cmd)
			if err != nil {
				return err
			}
			apps, err := client.ListApps(cmd.Context())
			if err != nil {
				return err
			}
			if asJSON {
				return printJSON(cmd.OutOrStdout(), apps)
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 2, 2, ' ', 0)
			fmt.Fprintln(tw, "SLUG\tSTATUS\tIMAGE\tPORT")
			for _, a := range apps {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%d\n", a.Slug, a.Status, a.Image, a.Port)
			}
			return tw.Flush()
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "output raw JSON instead of a table")
	addContextFlag(cmd)
	return cmd
}

// newDeployCmd builds `basepod deploy [path] -a/--app <slug> [--image <ref>] [--detach]`.
func newDeployCmd() *cobra.Command {
	var appSlug, image string
	var detach, fromGit bool
	cmd := &cobra.Command{
		Use:          "deploy [path]",
		Short:        "Deploy an app from a local build context, an image reference, or its connected git repo",
		Args:         cobra.MaximumNArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if fromGit && image != "" {
				return fmt.Errorf("--git and --image are mutually exclusive")
			}

			path := deployPathArg(args)
			if appSlug == "" {
				slug, mErr := appSlugFromManifest(path)
				if mErr != nil {
					return mErr
				}
				appSlug = slug
			}
			if appSlug == "" {
				return fmt.Errorf("--app/-a is required (or add a `name:` to basepod.yaml in %s)", path)
			}
			client, err := resolveClient(cmd)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()

			// The deploy-by-image path is unchanged: it hits the
			// synchronous POST .../deploy endpoint (still fast — no
			// build/upload involved), whose response is always already a
			// terminal status, so there's nothing to follow. --detach has
			// no effect here.
			if image != "" {
				dep, err := client.Deploy(cmd.Context(), appSlug, image)
				if err != nil {
					return err
				}
				printDeployment(out, dep)
				if dep.Status != "healthy" {
					return fmt.Errorf("deploy finished with status %q", dep.Status)
				}
				return nil
			}

			// --git triggers the app's connected repo (POST
			// .../deploy/git): the server clones synchronously — a bad
			// connection/branch surfaces here as an ordinary error — then
			// hands off to the same async build pipeline DeployTarball
			// uses, so the follow/poll flow below is shared with the
			// build-from-source path.
			if fromGit {
				dep, err := client.DeployGit(cmd.Context(), appSlug)
				if err != nil {
					return err
				}
				fmt.Fprintf(out, "Deployment #%d created from %s — building...\n", dep.Number, shortSHA(dep.GitSha))
				if detach {
					fmt.Fprintf(out, "deployment #%d: %s\n", dep.Number, dep.Status)
					return nil
				}
				return followDeployment(cmd, client, appSlug, dep.Number)
			}

			return deployFromSource(cmd, client, appSlug, path, detach)
		},
	}
	cmd.Flags().StringVarP(&appSlug, "app", "a", "", "app slug to deploy (default: the `name` in basepod.yaml, if present)")
	cmd.Flags().StringVar(&image, "image", "", "deploy this image reference instead of building from source")
	cmd.Flags().BoolVar(&fromGit, "git", false, "deploy the app's connected git repo instead of building from a local path")
	cmd.Flags().BoolVar(&detach, "detach", false, "for a build-from-source or --git deploy: print the deployment number and exit immediately, without following the build log or waiting for it to finish")
	addContextFlag(cmd)
	return cmd
}

// shortSHA truncates a full git commit SHA to its conventional 7-character
// short form for display — "" (an unresolved/non-git deployment) passes
// through unchanged rather than becoming a confusing 7-char slice of
// nothing.
func shortSHA(sha string) string {
	if len(sha) <= 7 {
		return sha
	}
	return sha[:7]
}

func deployPathArg(args []string) string {
	if len(args) == 1 {
		return args[0]
	}
	return "."
}

// appSlugFromManifest reads dir/basepod.yaml (or basepod.yml) if present
// and returns its `name` field, so `basepod deploy` can default --app
// for a project that already has one — the whole point of zero-config:
// a fresh directory needs no flags once `basepod init` (or a
// hand-written manifest) has set a name.
//
// slug == "" && err == nil means no manifest file exists at all — not
// an error, deploy just falls back to requiring --app. A manifest that
// exists but fails to parse, or parses without a name, IS reported as
// an error rather than silently falling through to "--app is required":
// the file is clearly meant to configure the app, so a broken or
// incomplete one deserves a message naming the actual problem.
func appSlugFromManifest(dir string) (slug string, err error) {
	for _, name := range []string{"basepod.yaml", "basepod.yml"} {
		f, openErr := os.Open(filepath.Join(dir, name))
		if openErr != nil {
			if os.IsNotExist(openErr) {
				continue
			}
			return "", fmt.Errorf("read %s: %w", name, openErr)
		}
		mf, _, parseErr := manifest.Parse(f)
		f.Close()
		if parseErr != nil {
			return "", fmt.Errorf("%s: %w", name, parseErr)
		}
		if mf.Name == "" {
			return "", fmt.Errorf("%s has no `name` field; pass --app explicitly", name)
		}
		return mf.Name, nil
	}
	return "", nil
}

// deployFromSource implements the tarball half of `basepod deploy`: pack
// path into a gzipped tar (respecting .basepodignore) and upload it. The
// server spools and validates the upload synchronously — a bad upload
// (413/422) still fails fast right here, exactly as before — but responds
// as soon as that succeeds, before the build even starts (see
// internal/api.handleDeployTarball); the deployment it returns is always
// still "deploying". Unless detach is set, this then follows that
// deployment to a terminal status: streaming its build log live (see
// followDeployment) and polling until it finishes, returning a non-nil
// error naming the deployment's own failure reason if it didn't land
// healthy. With detach, it prints the deployment number and returns
// immediately without waiting for any of that.
func deployFromSource(cmd *cobra.Command, client *Client, appSlug, path string, detach bool) error {
	out := cmd.OutOrStdout()

	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("deploy path %s: %w", path, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("deploy path %s is not a directory", path)
	}
	if !hasContainerfile(path) {
		return fmt.Errorf("no Containerfile or Dockerfile found at the root of %s", path)
	}

	f, size, err := packToTempFile(path)
	if err != nil {
		return fmt.Errorf("pack build context: %w", err)
	}
	defer func() {
		f.Close()
		os.Remove(f.Name())
	}()

	if size > sizeWarnThreshold {
		fmt.Fprintf(cmd.ErrOrStderr(), "warning: build context is %s (over 64 MiB) — this may take a while to upload\n", humanBytes(size))
	}

	fmt.Fprintf(out, "Uploading %s...\n", humanBytes(size))

	dep, err := client.DeployTarball(cmd.Context(), appSlug, f, size)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "Deployment #%d created — building...\n", dep.Number)

	if detach {
		fmt.Fprintf(out, "deployment #%d: %s\n", dep.Number, dep.Status)
		return nil
	}

	return followDeployment(cmd, client, appSlug, dep.Number)
}

// deployPollInterval is how often followDeployment re-polls GET
// .../deployments/{n} while a build-from-source deploy is still in
// progress. A package-level var (not a const) so tests can shrink it to
// keep the suite fast, matching internal/deploy's own probeInterval
// pattern.
var deployPollInterval = 2 * time.Second

// followDeployment is the async half of `basepod deploy`'s build-from-
// source flow (see deployFromSource): it streams the deployment's build
// log live to stdout — via a minted "build_log" stream token, exactly the
// credential model the dashboard's own SSE connections use (see
// internal/api/stream_token.go) rather than a bare session token in a URL
// — and then polls GET .../deployments/{number} until it reaches a
// terminal status. A failure streaming the log is only a warning (printed
// to stderr): the deploy itself may still be proceeding just fine, and
// polling below is what actually decides this command's outcome. Returns
// a non-nil error (naming the deployment's own Error message, if any) when
// the deployment didn't land "healthy".
func followDeployment(cmd *cobra.Command, client *Client, appSlug string, number int) error {
	out := cmd.OutOrStdout()
	ctx := cmd.Context()

	if err := streamBuildLog(ctx, client, appSlug, number, out); err != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "warning: could not stream the build log live: %v\n", err)
	}

	dep, err := pollDeploymentUntilTerminal(ctx, client, appSlug, number)
	if err != nil {
		return err
	}

	printDeployment(out, dep)
	if dep.Status != "healthy" {
		if dep.Error != "" {
			return errors.New(dep.Error)
		}
		return fmt.Errorf("deploy finished with status %q", dep.Status)
	}
	return nil
}

// streamBuildLog mints a "build_log" stream token for (appSlug, number) and
// opens its build-log route, printing every line to out as it arrives.
// The server serves this route two different ways depending on whether the
// deployment was still "deploying" at the moment the request landed (see
// internal/api.handleDeploymentLog): an SSE stream of "event: log" lines
// (raw text payloads — NOT JSON, unlike the container-log stream; see
// BuildLogPanel.vue's own doc comment for the same wire-format note) that
// the server itself closes once the deployment reaches a terminal status,
// or — if the build had already finished by the time this connects — a
// single plain-text body. Content-Type distinguishes the two; either way
// this returns once the response body is fully read/closed, so a caller
// doesn't need to guess which shape it got before moving on to polling.
func streamBuildLog(ctx context.Context, client *Client, appSlug string, number int, out io.Writer) error {
	token, err := client.CreateBuildLogStreamToken(ctx, appSlug, number)
	if err != nil {
		return fmt.Errorf("mint build-log stream token: %w", err)
	}

	resp, err := client.OpenDeploymentLog(ctx, appSlug, number, token.Token)
	if err != nil {
		return fmt.Errorf("open build log: %w", err)
	}
	defer resp.Body.Close()

	if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		return readSSE(resp.Body, func(ev sseEvent) bool {
			if ev.event == "log" {
				fmt.Fprintln(out, ev.data)
			}
			return true
		})
	}

	_, err = io.Copy(out, resp.Body)
	return err
}

// pollDeploymentUntilTerminal calls client.GetDeployment every
// deployPollInterval until its Status is no longer "deploying" (or ctx is
// done), returning the first terminal snapshot.
func pollDeploymentUntilTerminal(ctx context.Context, client *Client, appSlug string, number int) (*Deployment, error) {
	for {
		dep, err := client.GetDeployment(ctx, appSlug, number)
		if err != nil {
			return nil, err
		}
		if dep.Status != "deploying" {
			return dep, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(deployPollInterval):
		}
	}
}

// newComposeCmd builds `basepod compose` and its `up` subcommand (v0.5
// Task 8).
func newComposeCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "compose",
		Short: "Deploy a compose.yaml project as one BasePod app per service",
	}
	cmd.AddCommand(newComposeUpCmd())
	return cmd
}

// newComposeUpCmd builds `basepod compose up [path] [--project name] [--dry-run]`.
func newComposeUpCmd() *cobra.Command {
	var project string
	var dryRun bool
	cmd := &cobra.Command{
		Use:          "up [path]",
		Short:        "Parse and apply a compose.yaml (or compose.yml/docker-compose.yml) project",
		Args:         cobra.MaximumNArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			path := deployPathArg(args)
			client, err := resolveClient(cmd)
			if err != nil {
				return err
			}
			return composeUp(cmd, client, path, project, dryRun)
		},
	}
	cmd.Flags().StringVar(&project, "project", "", "compose project name (default: the compose file's top-level `name:`)")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "preview the plan without deploying anything")
	addContextFlag(cmd)
	return cmd
}

// composeUp implements `basepod compose up`: pack path (respecting
// .basepodignore, same packer `basepod deploy` uses) into a single
// gzipped tar — compose.yaml plus every service's own build context, all
// as one upload, matching what POST /compose/up expects — upload it, and
// print the resulting plan. For a real (non-dry-run) apply, it then
// follows every service's deployment to a terminal status, in the same
// dependency order the server deployed them, and returns a non-nil error
// naming which service(s) failed if any did — matching `basepod deploy`'s
// own "exit non-zero on failure" contract.
func composeUp(cmd *cobra.Command, client *Client, path, project string, dryRun bool) error {
	out := cmd.OutOrStdout()

	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("compose path %s: %w", path, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("compose path %s is not a directory", path)
	}

	f, size, err := packToTempFile(path)
	if err != nil {
		return fmt.Errorf("pack compose project: %w", err)
	}
	defer func() {
		f.Close()
		os.Remove(f.Name())
	}()

	if size > sizeWarnThreshold {
		fmt.Fprintf(cmd.ErrOrStderr(), "warning: upload is %s (over 64 MiB) — this may take a while to upload\n", humanBytes(size))
	}
	fmt.Fprintf(out, "Uploading %s...\n", humanBytes(size))

	result, err := client.ComposeUp(cmd.Context(), project, dryRun, f, size)
	if err != nil {
		return err
	}

	printComposePlan(out, result)

	if dryRun {
		return nil
	}
	return followComposeServices(cmd, client, result)
}

// printComposePlan prints a compose apply/dry-run response as a table
// (one row per service: name, slug, action, routing, deploy strategy)
// followed by every warning — per-service, then plan-level — and every
// orphaned service, so nothing the server flagged is silently invisible
// (doc 06's "warn loudly, never silently drop" ruling applies to this CLI
// output too, not just the future dashboard preview).
func printComposePlan(out io.Writer, result *ComposeResult) {
	fmt.Fprintf(out, "project: %s", result.Project)
	if result.DryRun {
		fmt.Fprint(out, " (dry run — nothing changed)")
	}
	fmt.Fprintln(out)

	tw := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "SERVICE\tSLUG\tACTION\tROUTING\tSTRATEGY")
	for _, s := range result.Services {
		routing := fmt.Sprintf("port %d", s.Port)
		if s.Internal {
			routing = "internal"
		}
		strategy := s.DeployStrategy
		if strategy == "" {
			strategy = "-"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", s.Name, s.Slug, s.Action, routing, strategy)
	}
	tw.Flush()

	for _, s := range result.Services {
		for _, w := range s.Warnings {
			fmt.Fprintf(out, "warning (%s): %s\n", s.Name, w)
		}
	}
	for _, w := range result.Warnings {
		fmt.Fprintf(out, "warning: %s\n", w)
	}
	for _, o := range result.Orphans {
		fmt.Fprintf(out, "orphan: %s is no longer in the compose file — left running untouched; delete it explicitly if it's no longer needed\n", o)
	}
}

// followComposeServices polls each service's deployment (in the same
// dependency order the response lists them, matching what the server
// actually deployed in) until it reaches a terminal status, printing each
// as it lands. A service with DeploymentNumber 0 was never reached (an
// earlier service in the chain failed — see internal/api/compose.go's
// partial-failure contract) and is skipped, its own deployment record
// (status "failed", "aborted: ...") already speaking for itself via the
// per-service loop above having printed nothing further to add here.
func followComposeServices(cmd *cobra.Command, client *Client, result *ComposeResult) error {
	out := cmd.OutOrStdout()
	var failed []string
	for _, s := range result.Services {
		if s.DeploymentNumber == 0 {
			continue
		}
		fmt.Fprintf(out, "%s: waiting for deployment #%d...\n", s.Name, s.DeploymentNumber)
		dep, err := pollDeploymentUntilTerminal(cmd.Context(), client, s.Slug, s.DeploymentNumber)
		if err != nil {
			return fmt.Errorf("%s: %w", s.Name, err)
		}
		printDeployment(out, dep)
		if dep.Status != "healthy" {
			failed = append(failed, s.Name)
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("compose apply finished with failed service(s): %s", strings.Join(failed, ", "))
	}
	return nil
}

// newLogsCmd builds `basepod logs <slug> [-f] [--tail N]`.
func newLogsCmd() *cobra.Command {
	var follow bool
	var tail int
	cmd := &cobra.Command{
		Use:          "logs <slug>",
		Short:        "Stream an app's container logs",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := resolveClient(cmd)
			if err != nil {
				return err
			}

			ctx := cmd.Context()
			if follow {
				// -f keeps the stream open until Ctrl-C: cancel the
				// request context on SIGINT so the read below unblocks
				// and we exit cleanly rather than needing to be killed.
				var stop func()
				ctx, stop = signal.NotifyContext(ctx, os.Interrupt)
				defer stop()
			}

			body, err := client.LogsStream(ctx, args[0], follow, tail)
			if err != nil {
				return err
			}
			defer body.Close()

			out := cmd.OutOrStdout()
			readErr := readSSE(body, func(ev sseEvent) bool {
				if line, ok := parseLogLine(ev); ok {
					fmt.Fprintln(out, line)
				}
				return true
			})
			// A read error caused by our own SIGINT cancellation (the -f
			// case) is the expected way to stop following, not a failure
			// worth reporting.
			if readErr != nil && ctx.Err() == nil {
				return readErr
			}
			return nil
		},
	}
	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "keep streaming new log lines until interrupted")
	cmd.Flags().IntVar(&tail, "tail", 0, "number of historical lines to show (server default if omitted)")
	addContextFlag(cmd)
	return cmd
}

// newEnvCmd builds `basepod env <slug>` and its `set`/`unset` subcommands.
func newEnvCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:          "env <slug>",
		Short:        "Show an app's environment variables",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := resolveClient(cmd)
			if err != nil {
				return err
			}
			vars, err := client.GetEnv(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if asJSON {
				return printJSON(cmd.OutOrStdout(), vars)
			}
			printEnvTable(cmd.OutOrStdout(), vars)
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "output raw JSON instead of KEY=VALUE lines")
	addContextFlag(cmd)
	cmd.AddCommand(newEnvSetCmd(), newEnvUnsetCmd())
	return cmd
}

// envAssignment is one parsed "KEY=VALUE" argument to `env set`.
type envAssignment struct {
	Key, Value string
}

// parseEnvAssignments parses each "KEY=VALUE" argument, splitting on the
// first "=" only (so a value may itself contain "=").
func parseEnvAssignments(args []string) ([]envAssignment, error) {
	out := make([]envAssignment, 0, len(args))
	for _, a := range args {
		idx := strings.IndexByte(a, '=')
		if idx < 0 {
			return nil, fmt.Errorf("invalid KEY=VALUE assignment %q", a)
		}
		out = append(out, envAssignment{Key: a[:idx], Value: a[idx+1:]})
	}
	return out, nil
}

// mergeEnvSet applies assignments on top of current (the app's existing
// env vars as returned by GetEnv), preserving every untouched key's entry
// exactly as-is — including a secret's masked "" value, which the server's
// keep-on-empty-secret contract (see EnvVar's doc comment) treats as "no
// change" rather than clearing it. Every key being set gets IsSecret ==
// secret: false ("new keys plain") unless the caller passed --secret, in
// which case every key in this invocation — new or pre-existing — is
// marked secret, matching the task brief's "--secret marks ALL keys being
// set as secret".
func mergeEnvSet(current []EnvVar, assignments []envAssignment, secret bool) []EnvVar {
	byKey := make(map[string]EnvVar, len(current)+len(assignments))
	order := make([]string, 0, len(current)+len(assignments))
	for _, v := range current {
		byKey[v.Key] = v
		order = append(order, v.Key)
	}
	for _, a := range assignments {
		if _, exists := byKey[a.Key]; !exists {
			order = append(order, a.Key)
		}
		byKey[a.Key] = EnvVar{Key: a.Key, Value: a.Value, IsSecret: secret}
	}
	out := make([]EnvVar, 0, len(order))
	for _, k := range order {
		out = append(out, byKey[k])
	}
	return out
}

func newEnvSetCmd() *cobra.Command {
	var secret bool
	cmd := &cobra.Command{
		Use:          "set <slug> KEY=VALUE [KEY=VALUE...]",
		Short:        "Set one or more environment variables",
		Args:         cobra.MinimumNArgs(2),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			slug := args[0]
			assignments, err := parseEnvAssignments(args[1:])
			if err != nil {
				return err
			}

			client, err := resolveClient(cmd)
			if err != nil {
				return err
			}
			current, err := client.GetEnv(cmd.Context(), slug)
			if err != nil {
				return err
			}

			merged := mergeEnvSet(current, assignments, secret)

			out, err := client.PutEnv(cmd.Context(), slug, merged)
			if err != nil {
				return err
			}
			printEnvTable(cmd.OutOrStdout(), out)
			return nil
		},
	}
	cmd.Flags().BoolVar(&secret, "secret", false, "mark every key being set as secret")
	addContextFlag(cmd)
	return cmd
}

func newEnvUnsetCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:          "unset <slug> KEY [KEY...]",
		Short:        "Remove one or more environment variables",
		Args:         cobra.MinimumNArgs(2),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			slug := args[0]
			remove := make(map[string]bool, len(args)-1)
			for _, k := range args[1:] {
				remove[k] = true
			}

			client, err := resolveClient(cmd)
			if err != nil {
				return err
			}
			current, err := client.GetEnv(cmd.Context(), slug)
			if err != nil {
				return err
			}

			kept := make([]EnvVar, 0, len(current))
			for _, v := range current {
				if !remove[v.Key] {
					kept = append(kept, v)
				}
			}

			out, err := client.PutEnv(cmd.Context(), slug, kept)
			if err != nil {
				return err
			}
			printEnvTable(cmd.OutOrStdout(), out)
			return nil
		},
	}
	addContextFlag(cmd)
	return cmd
}

// newRollbackCmd builds `basepod rollback <slug> <number>`.
func newRollbackCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:          "rollback <slug> <number>",
		Short:        "Roll back an app to an earlier deployment's exact image",
		Args:         cobra.ExactArgs(2),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			number, err := strconv.Atoi(args[1])
			if err != nil {
				return fmt.Errorf("invalid deployment number %q", args[1])
			}
			client, err := resolveClient(cmd)
			if err != nil {
				return err
			}
			dep, err := client.Rollback(cmd.Context(), args[0], number)
			if err != nil {
				return err
			}
			printDeployment(cmd.OutOrStdout(), dep)
			return nil
		},
	}
	addContextFlag(cmd)
	return cmd
}

// statusOutput is the --json shape of `basepod status`.
type statusOutput struct {
	System SystemInfo `json:"system"`
	Apps   []AppInfo  `json:"apps"`
}

// newStatusCmd builds `basepod status`.
func newStatusCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:          "status",
		Short:        "Show system and app status",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := resolveClient(cmd)
			if err != nil {
				return err
			}
			sys, err := client.System(cmd.Context())
			if err != nil {
				return err
			}
			apps, err := client.ListApps(cmd.Context())
			if err != nil {
				return err
			}

			out := cmd.OutOrStdout()
			if asJSON {
				return printJSON(out, statusOutput{System: *sys, Apps: apps})
			}
			fmt.Fprintf(out, "version: %s\npodman:  %s\napps:    %d\n\n", sys.Version, sys.Podman, sys.Apps)
			tw := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
			fmt.Fprintln(tw, "SLUG\tSTATUS\tIMAGE\tPORT")
			for _, a := range apps {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%d\n", a.Slug, a.Status, a.Image, a.Port)
			}
			return tw.Flush()
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "output raw JSON instead of a table")
	addContextFlag(cmd)
	return cmd
}

// newGitCmd builds `basepod git connect|status|disconnect` — the config
// half of git push-to-deploy (v0.5 plan Task 4); `basepod deploy --git`
// triggers the manual deploy itself (see newDeployCmd).
func newGitCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "git",
		Short: "Manage an app's connected git repo (push-to-deploy)",
	}
	cmd.AddCommand(newGitConnectCmd(), newGitStatusCmd(), newGitDisconnectCmd())
	return cmd
}

// printGitSource prints a connected repo's config and webhook setup
// instructions: the URL/branch, whether a deploy token is set, and the
// webhook URL + secret an operator pastes into GitHub/GitLab/Gitea's
// webhook settings (payload URL: WebhookURL, content type
// application/json, secret: Secret — see gitSourceResponse's doc comment
// for why the secret is readable here, unlike the deploy token).
func printGitSource(w io.Writer, gs *GitSource) {
	fmt.Fprintf(w, "url:      %s\n", gs.URL)
	fmt.Fprintf(w, "branch:   %s\n", gs.Branch)
	if gs.Provider != "" {
		fmt.Fprintf(w, "provider: %s\n", gs.Provider)
	}
	token := "not set"
	if gs.Token == "set" {
		token = "set"
	}
	fmt.Fprintf(w, "token:    %s\n", token)
	fmt.Fprintf(w, "\nwebhook (add this to your forge's webhook settings):\n")
	fmt.Fprintf(w, "  payload URL:   %s\n", gs.WebhookURL)
	fmt.Fprintf(w, "  content type:  application/json\n")
	fmt.Fprintf(w, "  secret:        %s\n", gs.Secret)
	for _, warning := range gs.Warnings {
		fmt.Fprintf(w, "\nwarning: %s\n", warning)
	}
}

// newGitConnectCmd builds `basepod git connect <slug> --url --branch [--token] [--rotate-secret]`.
func newGitConnectCmd() *cobra.Command {
	var url, branch, token string
	var rotateSecret bool
	cmd := &cobra.Command{
		Use:          "connect <slug>",
		Short:        "Connect (or reconfigure) an app's git repo for push-to-deploy",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if url == "" || branch == "" {
				return fmt.Errorf("--url and --branch are required")
			}
			client, err := resolveClient(cmd)
			if err != nil {
				return err
			}
			gs, err := client.PutGitSource(cmd.Context(), args[0], url, branch, token, rotateSecret)
			if err != nil {
				return err
			}
			printGitSource(cmd.OutOrStdout(), gs)
			return nil
		},
	}
	cmd.Flags().StringVar(&url, "url", "", "the repo's https clone URL (required)")
	cmd.Flags().StringVar(&branch, "branch", "", "the branch to deploy on push (required)")
	cmd.Flags().StringVar(&token, "token", "", "a deploy token for a private repo (omit to keep any already-stored token unchanged)")
	cmd.Flags().BoolVar(&rotateSecret, "rotate-secret", false, "mint a fresh webhook hook_id and secret, invalidating the old webhook URL")
	addContextFlag(cmd)
	return cmd
}

// newGitStatusCmd builds `basepod git status <slug>` — the connected
// repo's config plus its most recent webhook deliveries.
func newGitStatusCmd() *cobra.Command {
	var asJSON bool
	var limit int
	cmd := &cobra.Command{
		Use:          "status <slug>",
		Short:        "Show an app's connected git repo and recent webhook deliveries",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := resolveClient(cmd)
			if err != nil {
				return err
			}
			gs, err := client.GetGitSource(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			deliveries, err := client.ListGitDeliveries(cmd.Context(), args[0], limit)
			if err != nil {
				return err
			}

			out := cmd.OutOrStdout()
			if asJSON {
				return printJSON(out, struct {
					GitSource
					Deliveries []GitDelivery `json:"deliveries"`
				}{GitSource: *gs, Deliveries: deliveries})
			}

			printGitSource(out, gs)
			fmt.Fprintf(out, "\nrecent deliveries:\n")
			if len(deliveries) == 0 {
				fmt.Fprintf(out, "  (none yet)\n")
				return nil
			}
			tw := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
			fmt.Fprintln(tw, "RECEIVED\tEVENT\tREF\tSHA\tSTATUS\tDEPLOYMENT")
			for _, d := range deliveries {
				deployment := "-"
				if d.DeploymentNumber != nil {
					deployment = "#" + strconv.Itoa(*d.DeploymentNumber)
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
					d.ReceivedAt, d.Event, d.Ref, shortSHA(d.CommitSHA), d.Status, deployment)
			}
			return tw.Flush()
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "output raw JSON instead of a table")
	cmd.Flags().IntVar(&limit, "limit", 20, "how many recent deliveries to show")
	addContextFlag(cmd)
	return cmd
}

// newGitDisconnectCmd builds `basepod git disconnect <slug>`.
func newGitDisconnectCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:          "disconnect <slug>",
		Short:        "Disconnect an app's git repo",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := resolveClient(cmd)
			if err != nil {
				return err
			}
			if err := client.DeleteGitSource(cmd.Context(), args[0]); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "disconnected %s\n", args[0])
			return nil
		},
	}
	addContextFlag(cmd)
	return cmd
}
