package cli

import (
	"errors"
	"fmt"
	"net/url"
	"os"

	"github.com/spf13/cobra"

	"github.com/plexusone/deployforge/deploy/cfranges"
	"github.com/plexusone/deployforge/fleet"
	"github.com/plexusone/deployforge/tlsmin"
	"github.com/plexusone/deployforge/zones"
)

// zonesCmd checks the zones document, verifies it against the zones' name
// servers, writes its schema and page, and hardens the zones.
func zonesCmd(loadFleet func() (*fleet.Fleet, error)) *cobra.Command {
	var path string
	load := func() (*zones.Document, error) {
		d, err := zones.Load(path)
		if err != nil {
			return nil, err
		}
		f, err := loadFleet()
		if err != nil {
			return nil, err
		}
		return d, d.CheckApps(f)
	}
	root := &cobra.Command{
		Use:   "zones",
		Short: "Check the DNS zones in the zones document, verify them against their name servers, and harden them",
	}
	root.PersistentFlags().StringVar(&path, "zones", "zones.json", "the zones document")

	root.AddCommand(&cobra.Command{
		Use:   "validate",
		Short: "Check the zones document, and that every live host name of the fleet has a record",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			d, err := load()
			if err != nil {
				return err
			}
			n := 0
			for _, z := range d.Zones {
				n += len(z.Records)
			}
			_, err = fmt.Fprintf(c.OutOrStdout(), "%s: %d zones, %d records OK\n", path, len(d.Zones), n)
			return err
		},
	})

	var recordsOnly, failPlanned bool
	verify := &cobra.Command{
		Use:   "verify [zone...]",
		Short: "Ask each zone's name servers for every listed record, and make the records' HTTPS checks",
		RunE: func(c *cobra.Command, args []string) error {
			d, err := load()
			if err != nil {
				return err
			}
			ranges, err := cfranges.Ranges()
			if err != nil {
				return err
			}
			v := &zones.Verifier{Cloudflare: ranges, RecordsOnly: recordsOnly, FailPlanned: failPlanned}
			found, err := v.Verify(c.Context(), d, args...)
			if err != nil {
				return err
			}
			failed, zone := 0, ""
			for _, f := range found {
				if f.Zone != zone {
					zone = f.Zone
					if _, err := fmt.Fprintf(c.OutOrStdout(), "%s\n", zone); err != nil {
						return err
					}
				}
				if f.Outcome == zones.Fail {
					failed++
				}
				if _, err := fmt.Fprintf(c.OutOrStdout(), "  %s\n", f); err != nil {
					return err
				}
			}
			if failed > 0 {
				return fmt.Errorf("%d record(s) or check(s) failed", failed)
			}
			return nil
		},
	}
	verify.Flags().BoolVar(&recordsOnly, "records-only", false, "check the records but not their HTTPS checks, as before a planned change while a host is down")
	verify.Flags().BoolVar(&failPlanned, "fail-planned", false, "fail a planned record instead of skipping it, as after a step that adds records")
	root.AddCommand(verify)

	var getStatus int
	var getLocation string
	get := &cobra.Command{
		Use:   "get <https-url>",
		Short: "GET a URL on a zone's host at the addresses the zone's name servers answer, and check its status",
		Long: "get resolves the host at its zone's own name servers, as verify does, so a record added moments ago\n" +
			"is reached even while a resolver still caches its absence. It exits non-zero unless the response has\n" +
			"--status, and a Location starting with --location when that is set.",
		Args: cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			d, err := load()
			if err != nil {
				return err
			}
			u, err := url.Parse(args[0])
			if err != nil || u.Scheme != "https" || u.Host == "" || u.Port() != "" {
				return fmt.Errorf("zones get: %q is not an https URL on the default port", args[0])
			}
			zone, ok := d.ZoneOf(u.Hostname())
			if !ok {
				return fmt.Errorf("zones get: %s is in no zone of %s", u.Hostname(), path)
			}
			p := u.EscapedPath()
			if p == "" {
				p = "/"
			}
			if u.RawQuery != "" {
				p += "?" + u.RawQuery
			}
			v := &zones.Verifier{}
			detail, err := v.Get(c.Context(), zone, u.Hostname(), &zones.HTTPCheck{Path: p, Status: getStatus, Location: getLocation})
			if err != nil {
				return err
			}
			_, err = fmt.Fprintln(c.OutOrStdout(), detail)
			return err
		},
	}
	get.Flags().IntVar(&getStatus, "status", 200, "the status it must answer")
	get.Flags().StringVar(&getLocation, "location", "", "what a redirect's Location must start with")
	root.AddCommand(get)

	var schemaOut string
	schemaCmd := &cobra.Command{
		Use:   "schema",
		Short: "Write the zones document's JSON Schema, generated from the Go types",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			out, err := zones.JSONSchema()
			if err != nil {
				return err
			}
			return writeOut(c, schemaOut, out)
		},
	}
	schemaCmd.Flags().StringVarP(&schemaOut, "output", "o", "", "file to write; standard output when empty")
	root.AddCommand(schemaCmd)

	var pageOut string
	page := &cobra.Command{
		Use:   "page",
		Short: "Write a Markdown page of the zones document",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			d, err := load()
			if err != nil && d == nil {
				return err
			}
			if err != nil {
				return errors.Join(errors.New("the zones document does not match the fleet"), err)
			}
			return writeOut(c, pageOut, []byte(d.Page()))
		},
	}
	page.Flags().StringVarP(&pageOut, "output", "o", "", "file to write; standard output when empty")
	root.AddCommand(page)
	root.AddCommand(hardenCmd(load))
	root.AddCommand(&cobra.Command{
		Use:   "tls [<host>...]",
		Short: "Check from outside that each host refuses TLS 1.1 and accepts TLS 1.2; default every live stack's host names",
		RunE: func(c *cobra.Command, hosts []string) error {
			if len(hosts) == 0 {
				f, err := loadFleet()
				if err != nil {
					return err
				}
				hosts = liveHostNames(f)
			}
			var errs []error
			for _, h := range hosts {
				if err := tlsmin.Check(c.Context(), h, nil); err != nil {
					errs = append(errs, err)
					continue
				}
				if _, err := fmt.Fprintf(c.OutOrStdout(), "%s refuses TLS 1.1 and accepts TLS 1.2\n", h); err != nil {
					return err
				}
			}
			return errors.Join(errs...)
		},
	})
	return root
}

// writeOut writes data to path, or to standard output when path is empty.
func writeOut(c *cobra.Command, path string, data []byte) error {
	if path == "" {
		_, err := c.OutOrStdout().Write(data)
		return err
	}
	if err := os.WriteFile(path, data, 0o644); err != nil { //nolint:gosec // G306: a generated document, meant to be readable
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// liveHostNames are the host names of every live stack, in fleet order.
func liveHostNames(f *fleet.Fleet) []string {
	var out []string
	for _, a := range f.Apps {
		for _, stack := range f.StackNames(a) {
			if s := a.Stacks[stack]; s.Status == fleet.Live {
				out = append(out, s.Hosts...)
			}
		}
	}
	return out
}
