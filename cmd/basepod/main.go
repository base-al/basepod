package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/base-al/basepod/internal/config"
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
	)
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
