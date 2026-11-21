package cli

import (
	"errors"
	"fmt"
	"os"

	cf "github.com/cloudflare/cloudflare-go/v7"
	"github.com/cloudflare/cloudflare-go/v7/option"
	"github.com/spf13/cobra"

	"github.com/plexusone/deployforge/zoneharden"
	"github.com/plexusone/deployforge/zones"
)

// TokenEnv holds the Cloudflare API token commands that call Cloudflare use.
const TokenEnv = "CLOUDFLARE_API_TOKEN" //nolint:gosec // G101: the name of an environment variable, not a credential

// hardenCmd applies the security baseline to zones through Cloudflare's
// API.
func hardenCmd(load func() (*zones.Document, error)) *cobra.Command {
	root := &cobra.Command{
		Use:   "harden",
		Short: "Plan, apply or verify the security baseline on zones through Cloudflare's API (token in " + TokenEnv + ")",
		Long: "harden sets each zone's TLS settings and writes its custom rules, rate limit, Free Managed Ruleset,\n" +
			"cache rule and www redirect, as the baseline and the zone's hardening in the zones document say. Its rules carry refs beginning with " + zoneharden.RefPrefix + "; a phase's other rules are kept.\n" +
			"The token needs Zone Read, Zone Settings Edit, Zone WAF Edit, Cache Rules Edit, Single Redirect Edit\n" +
			"and Bot Management Edit on the zones (Read is enough for plan and verify). Name the zones, or leave\n" +
			"them out for every zone with hardening.",
	}
	run := func(c *cobra.Command, mode zoneharden.Mode, names []string) error {
		token := os.Getenv(TokenEnv)
		if token == "" {
			return fmt.Errorf("zones harden: set %s to a Cloudflare API token on the zones", TokenEnv)
		}
		d, err := load()
		if err != nil {
			return err
		}
		var zs []zones.Zone
		for _, z := range d.Zones {
			named := len(names) == 0 && z.Hardening != nil
			for _, n := range names {
				named = named || n == z.Name
			}
			if named {
				zs = append(zs, z)
			}
		}
		if len(names) > 0 && len(zs) != len(names) {
			return fmt.Errorf("zones harden: %v names a zone the zones document does not have", names)
		}
		api := cf.NewClient(option.WithAPIToken(token))
		_, err = zoneharden.Run(c.Context(), api, mode, zs, c.OutOrStdout())
		return err
	}
	root.AddCommand(&cobra.Command{
		Use:   "plan [zone...]",
		Short: "Say what differs from the baseline and what apply would do; change nothing",
		RunE:  func(c *cobra.Command, args []string) error { return run(c, zoneharden.Plan, args) },
	})
	var confirm bool
	apply := &cobra.Command{
		Use:   "apply [zone...]",
		Short: "Change what differs, then verify; needs --confirm",
		RunE: func(c *cobra.Command, args []string) error {
			if !confirm {
				if err := run(c, zoneharden.Plan, args); err != nil {
					return err
				}
				return errors.New("zones harden: apply changes the zones' security settings; read the plan above, then run again with --confirm")
			}
			return run(c, zoneharden.Apply, args)
		},
	}
	apply.Flags().BoolVar(&confirm, "confirm", false, "apply the plan")
	root.AddCommand(apply)
	root.AddCommand(&cobra.Command{
		Use:   "verify [zone...]",
		Short: "Check every zone matches the baseline; exit non-zero if not",
		RunE:  func(c *cobra.Command, args []string) error { return run(c, zoneharden.Verify, args) },
	})
	return root
}
