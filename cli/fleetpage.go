package cli

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/plexusone/deployforge/fleet"
	"github.com/plexusone/deployforge/fleetpage"
)

// fleetCmd writes the fleet's Markdown pages (package fleetpage).
func fleetCmd(load func() (*fleet.Fleet, error), fleetPath *string) *cobra.Command {
	root := &cobra.Command{
		Use:   "fleet",
		Short: "Write the fleet's Markdown pages: its inventory, and the fleet document's format reference",
	}
	var inventoryOut, formatOut string
	inventory := &cobra.Command{
		Use:   "inventory",
		Short: "Write the fleet's inventory: each app's stacks, secrets, labels, units, limits and notes",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			f, err := load()
			if err != nil {
				return err
			}
			return writePage(c, inventoryOut, fleetpage.Inventory(f, *fleetPath, commandLine(c)))
		},
	}
	inventory.Flags().StringVarP(&inventoryOut, "output", "o", "", "file to write; standard output when empty")
	format := &cobra.Command{
		Use:   "format",
		Short: "Write the fleet document's format reference, from the Go types the schema is generated from",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			page, err := fleetpage.Format(commandLine(c))
			if err != nil {
				return err
			}
			return writePage(c, formatOut, page)
		},
	}
	format.Flags().StringVarP(&formatOut, "output", "o", "", "file to write; standard output when empty")
	root.AddCommand(inventory, format)
	return root
}

// commandLine is how a page names the command that wrote it.
func commandLine(c *cobra.Command) string {
	return strings.Join(strings.Fields(c.CommandPath()), " ")
}

// writePage writes a page to path, or to standard output when path is
// empty.
func writePage(c *cobra.Command, path, page string) error {
	if path == "" {
		_, err := fmt.Fprint(c.OutOrStdout(), page)
		return err
	}
	if err := os.WriteFile(path, []byte(page), 0o644); err != nil { //nolint:gosec // G306: a generated page, committed and public to the repository
		return fmt.Errorf("fleet: %w", err)
	}
	return nil
}
