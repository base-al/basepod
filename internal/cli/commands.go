package cli

import (
	"bufio"
	"fmt"
	"net/url"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
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
		newContextCmd(),
		newAppsCmd(),
		newDeployCmd(),
		newLogsCmd(),
		newEnvCmd(),
		newRollbackCmd(),
		newStatusCmd(),
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
	var email, password, ctxName string
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
			if password == "" {
				// No golang.org/x/term dependency here (see task brief):
				// the password is read as a plain line from stdin, echoed
				// like any other terminal input. Warn so that isn't a
				// surprise over e.g. a screen-shared terminal.
				fmt.Fprintln(out, "Warning: password will be echoed (typed input is not masked)")
				fmt.Fprint(out, "Password: ")
				line, err := readLine(in)
				if err != nil {
					return fmt.Errorf("read password: %w", err)
				}
				password = line
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
	cmd.Flags().StringVar(&password, "password", "", "account password (prompted if omitted)")
	cmd.Flags().StringVar(&ctxName, "context", "", "name for the saved context (default: the URL's host)")
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

func newContextListCmd() *cobra.Command {
	return &cobra.Command{
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

// newDeployCmd builds `basepod deploy [path] -a/--app <slug> [--image <ref>]`.
func newDeployCmd() *cobra.Command {
	var appSlug, image string
	cmd := &cobra.Command{
		Use:          "deploy [path]",
		Short:        "Deploy an app from a local build context, or an image reference",
		Args:         cobra.MaximumNArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if appSlug == "" {
				return fmt.Errorf("--app/-a is required")
			}
			client, err := resolveClient(cmd)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()

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

			return deployFromSource(cmd, client, appSlug, deployPathArg(args))
		},
	}
	cmd.Flags().StringVarP(&appSlug, "app", "a", "", "app slug to deploy (required)")
	cmd.Flags().StringVar(&image, "image", "", "deploy this image reference instead of building from source")
	addContextFlag(cmd)
	return cmd
}

func deployPathArg(args []string) string {
	if len(args) == 1 {
		return args[0]
	}
	return "."
}

// deployFromSource implements the tarball half of `basepod deploy`: pack
// path into a gzipped tar (respecting .basepodignore), upload it, and
// block until the server finishes building and rolling it out. Kept
// deliberately simple and synchronous rather than streaming live build
// logs — see the task brief's "keep v1 SIMPLE AND SYNC" note: on failure,
// it fetches the app's most recent deployment and prints its build log
// (if any) so the user sees why, rather than leaving them to go dig for
// it themselves.
func deployFromSource(cmd *cobra.Command, client *Client, appSlug, path string) error {
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
	fmt.Fprintln(out, "Building & deploying (this may take a few minutes)...")

	dep, deployErr := client.DeployTarball(cmd.Context(), appSlug, f, size)
	if deployErr != nil {
		printBuildLogOnFailure(cmd, client, appSlug)
		return deployErr
	}

	printDeployment(out, dep)
	if dep.Status != "healthy" {
		return fmt.Errorf("deploy finished with status %q", dep.Status)
	}
	return nil
}

// printBuildLogOnFailure looks up appSlug's most recent deployment (the
// one the just-failed tarball upload created) and prints its build log,
// best-effort — a failure here (network hiccup, the deployment somehow
// having no log) is swallowed rather than compounding the original deploy
// error.
func printBuildLogOnFailure(cmd *cobra.Command, client *Client, appSlug string) {
	detail, err := client.GetApp(cmd.Context(), appSlug)
	if err != nil || len(detail.Deployments) == 0 {
		return
	}
	latest := detail.Deployments[0]
	if !latest.HasBuildLog {
		return
	}
	logText, err := client.DeploymentLog(cmd.Context(), appSlug, latest.Number)
	if err != nil || logText == "" {
		return
	}
	out := cmd.OutOrStdout()
	fmt.Fprintln(out, "--- build log ---")
	fmt.Fprintln(out, logText)
	fmt.Fprintln(out, "--- end build log ---")
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

// newStatusCmd builds `basepod status`.
func newStatusCmd() *cobra.Command {
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
			fmt.Fprintf(out, "version: %s\npodman:  %s\napps:    %d\n\n", sys.Version, sys.Podman, sys.Apps)
			tw := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
			fmt.Fprintln(tw, "SLUG\tSTATUS\tIMAGE\tPORT")
			for _, a := range apps {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%d\n", a.Slug, a.Status, a.Image, a.Port)
			}
			return tw.Flush()
		},
	}
	addContextFlag(cmd)
	return cmd
}
