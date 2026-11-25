package cli

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/plexusone/deployforge/deploy/cfranges"
)

// cloudflareHTTP fetches Cloudflare's published ranges; nil is a client
// with a timeout. Tests point it at a fake server.
var cloudflareHTTP *http.Client

// cloudflareIPsCmd refreshes the committed Cloudflare ranges from the lists
// Cloudflare publishes, or checks that this build's ranges still match them.
func cloudflareIPsCmd() *cobra.Command {
	var dir string
	var check bool
	c := &cobra.Command{
		Use:   "cloudflare-ips",
		Short: "Refresh the committed Cloudflare ranges from Cloudflare's published lists, or check them with --check",
		Long: "The firewall and Caddy trust Cloudflare's proxy ranges, which deployforge carries in\n" +
			"deploy/cfranges/ips-v4.txt and ips-v6.txt. cloudflare-ips fetches Cloudflare's published lists and\n" +
			"writes them to --dir, for review and a release. --check writes nothing: it compares the ranges this\n" +
			"build carries with the published ones, and fails when Cloudflare has added or removed one.",
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			v4, v6, err := cfranges.Fetch(c.Context(), cloudflareHTTP)
			if err != nil {
				return err
			}
			if check {
				have, err := cfranges.Ranges()
				if err != nil {
					return err
				}
				added, removed := cfranges.Diff(have, append(v4, v6...))
				if len(added) == 0 && len(removed) == 0 {
					_, err := fmt.Fprintln(c.OutOrStdout(), "the committed ranges match Cloudflare's published ranges")
					return err
				}
				return fmt.Errorf("cloudflare-ips: the committed ranges are out of date: Cloudflare added %v and removed %v; run cloudflare-ips and release", added, removed)
			}
			if err := os.WriteFile(filepath.Join(dir, "ips-v4.txt"), []byte(cfranges.Format(v4, cfranges.URLv4)), 0o644); err != nil { //nolint:gosec // G306: public address ranges, meant to be readable
				return fmt.Errorf("cloudflare-ips: %w", err)
			}
			if err := os.WriteFile(filepath.Join(dir, "ips-v6.txt"), []byte(cfranges.Format(v6, cfranges.URLv6)), 0o644); err != nil { //nolint:gosec // G306: public address ranges, meant to be readable
				return fmt.Errorf("cloudflare-ips: %w", err)
			}
			_, err = fmt.Fprintf(c.OutOrStdout(), "wrote %d IPv4 and %d IPv6 ranges to %s\n", len(v4), len(v6), dir)
			return err
		},
	}
	c.Flags().StringVar(&dir, "dir", filepath.Join("deploy", "cfranges"), "directory of ips-v4.txt and ips-v6.txt")
	c.Flags().BoolVar(&check, "check", false, "compare this build's ranges with Cloudflare's published ones instead of writing them")
	return c
}
