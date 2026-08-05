package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

var Version = "dev" // set via -ldflags "-X main.Version=…"

func main() {
	root := &cobra.Command{Use: "basepod", Short: "BasePod — self-hosted PaaS on Podman + Caddy"}
	root.AddCommand(
		&cobra.Command{Use: "version", Short: "Print version", Run: func(*cobra.Command, []string) {
			fmt.Println(Version)
		}},
		newServerCmd(), // added in Task 8; stub now: prints "not implemented", exits 1
		newSetupCmd(),  // added in Task 7; stub now
	)
	if err := root.Execute(); err != nil {
		os.Exit(1)
	}
}

func newServerCmd() *cobra.Command {
	return &cobra.Command{Use: "server", Short: "Run the control plane", RunE: func(*cobra.Command, []string) error {
		return fmt.Errorf("not implemented yet")
	}}
}

func newSetupCmd() *cobra.Command {
	return &cobra.Command{Use: "setup", Short: "First-run setup", RunE: func(*cobra.Command, []string) error {
		return fmt.Errorf("not implemented yet")
	}}
}
