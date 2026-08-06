package main

import (
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/base-al/basepod/internal/cli"
	"github.com/base-al/basepod/internal/config"
	"github.com/base-al/basepod/internal/gitsource"
	"github.com/base-al/basepod/internal/server"
)

var Version = "dev" // set via -ldflags "-X main.Version=…"

func main() {
	root := &cobra.Command{Use: "basepod", Short: "BasePod — self-hosted PaaS on Podman + Caddy"}
	root.AddCommand(
		&cobra.Command{Use: "version", Short: "Print version", Run: func(*cobra.Command, []string) {
			fmt.Println(Version)
		}},
		newServerCmd(),
		newSetupCmd(),
		newGitAskpassCmd(),
	)
	root.AddCommand(cli.Commands()...)
	if err := root.Execute(); err != nil {
		os.Exit(1)
	}
}

func newServerCmd() *cobra.Command {
	var cfgPath string
	cmd := &cobra.Command{
		Use:   "server",
		Short: "Run the control plane",
		RunE: func(cmd *cobra.Command, _ []string) error {
			server.Version = Version
			return server.Run(cmd.Context(), cfgPath)
		},
	}
	cmd.Flags().StringVar(&cfgPath, "config", config.DefaultPath(), "config file")
	return cmd
}

// adminPasswordFlagWarning is printed to stderr whenever --admin-password
// is actually used (audit finding L4): a password passed as a CLI flag
// is visible to any other process on the host via `ps` for as long as
// `basepod setup` is running, and often persists in shell history
// afterward. The flag itself is kept — removing it breaks the documented
// setup flow — but this warning at least tells an operator an
// interactive prompt is the safer path, once one exists.
const adminPasswordFlagWarning = "warning: --admin-password is visible via `ps` and may be recorded in shell history; " +
	"prefer an interactive password prompt once one is available (see the BasePod docs)."

// warnAdminPasswordFlag writes adminPasswordFlagWarning to w if pw is
// non-empty (i.e. --admin-password was actually supplied). Split out from
// newSetupCmd's RunE so it's testable without exercising the rest of the
// setup pipeline (which touches the real filesystem/store).
func warnAdminPasswordFlag(w io.Writer, pw string) {
	if pw == "" {
		return
	}
	fmt.Fprintln(w, adminPasswordFlagWarning)
}

func newSetupCmd() *cobra.Command {
	var (
		cfgPath       string
		rootDomain    string
		adminEmail    string
		adminPassword string
		dataDir       string
	)
	cmd := &cobra.Command{
		Use:   "setup",
		Short: "First-run setup",
		RunE: func(*cobra.Command, []string) error {
			// Validate required flags
			if rootDomain == "" {
				return fmt.Errorf("--root-domain is required")
			}
			if adminEmail == "" {
				return fmt.Errorf("--admin-email is required")
			}
			if adminPassword == "" {
				return fmt.Errorf("--admin-password is required")
			}
			warnAdminPasswordFlag(os.Stderr, adminPassword)

			// If data-dir is empty, use the config default
			if dataDir == "" {
				cfg, _ := config.Load(cfgPath)
				dataDir = cfg.DataDir
			}

			// Call server.Setup
			if err := server.Setup(cfgPath, server.SetupOptions{
				RootDomain:    rootDomain,
				AdminEmail:    adminEmail,
				AdminPassword: adminPassword,
				DataDir:       dataDir,
			}); err != nil {
				return err
			}

			fmt.Println("Setup complete. Start with: basepod server")
			return nil
		},
	}

	cmd.Flags().StringVar(&cfgPath, "config", config.DefaultPath(), "config file")
	cmd.Flags().StringVar(&rootDomain, "root-domain", "", "root domain for deployed apps")
	cmd.Flags().StringVar(&adminEmail, "admin-email", "", "email address for admin user")
	cmd.Flags().StringVar(&adminPassword, "admin-password", "", "password for admin user (min 8 chars)")
	cmd.Flags().StringVar(&dataDir, "data-dir", "", "data directory (default: system data dir)")

	return cmd
}

// newGitAskpassCmd wires internal/gitsource.Askpass into a hidden
// subcommand: internal/gitsource.Cloner.Fetch points GIT_ASKPASS at a
// small wrapper script that re-execs this same basepod binary as
// `basepod internal-git-askpass <prompt>` for every credential prompt
// git itself raises during a private-repo clone. Hidden because it's
// never meant to be run directly by an operator — see gitsource's
// package doc comment for why the token reaches git this way (never in
// argv, the clone URL, or on-disk git config) rather than any simpler
// alternative.
func newGitAskpassCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "internal-git-askpass [prompt]",
		Hidden: true,
		Args:   cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var prompt string
			if len(args) > 0 {
				prompt = args[0]
			}
			fmt.Fprintln(cmd.OutOrStdout(), gitsource.Askpass(prompt))
			return nil
		},
	}
}
