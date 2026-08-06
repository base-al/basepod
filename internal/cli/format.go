package cli

import (
	"encoding/json"
	"fmt"
	"io"
)

// printJSON writes v as indented JSON to w — the shared implementation
// behind every read command's --json flag.
func printJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// printDeployment prints a one-line summary of a deploy/rollback result,
// plus its error text (if any) on a second line.
func printDeployment(w io.Writer, d *Deployment) {
	fmt.Fprintf(w, "deployment #%d: %s (%s)\n", d.Number, d.Status, d.Image)
	if d.Error != "" {
		fmt.Fprintf(w, "error: %s\n", d.Error)
	}
}

// secretMask is printed in place of a secret env var's value — BasePod
// never sends secret plaintext back to the client (see EnvVar's doc
// comment), so there is no real value to show here.
const secretMask = "●●●"

// printEnvTable prints one "KEY=VALUE" line per env var, masking secrets.
func printEnvTable(w io.Writer, vars []EnvVar) {
	for _, v := range vars {
		value := v.Value
		if v.IsSecret {
			value = secretMask
		}
		fmt.Fprintf(w, "%s=%s\n", v.Key, value)
	}
}

// humanBytes formats n bytes as a short human-readable size (e.g.
// "1.5 MiB"), binary (1024-based) units to match maxTarballBody/the 64 MiB
// warning threshold's own binary sizing.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for n/div >= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
