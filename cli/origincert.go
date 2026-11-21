package cli

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"time"

	"github.com/spf13/cobra"

	"github.com/plexusone/deployforge/deploy/caddy"
	"github.com/plexusone/deployforge/deploy/deployer"
	"github.com/plexusone/deployforge/fleet"
	"github.com/plexusone/deployforge/origincert"
)

// zoneName is a zone a certificate is issued for.
var zoneName = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]*[a-z0-9])?\.)+[a-z][a-z0-9-]*[a-z0-9]$`)

func originCertCmd(load func() (*fleet.Fleet, error), dial func(key string) deployer.Dialer, openAPI func() (origincert.API, error)) *cobra.Command {
	root := &cobra.Command{
		Use:   "origin-cert",
		Short: "Issue, install, list and revoke a zone's Cloudflare Origin CA certificates (token in " + TokenEnv + ")",
	}
	var out string
	var days int
	issue := &cobra.Command{
		Use:   "issue <zone>",
		Short: "Have Cloudflare sign the CSR on standard input as the zone's origin certificate, for the zone and *.zone",
		Long: "issue reads a PEM certificate signing request on standard input, made where the certificate will be\n" +
			"served so the private key never leaves it, and writes the signed certificate (PEM) to --output or\n" +
			"standard output. Its ID and expiry go to standard error.",
		Args: cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			api, err := openAPI()
			if err != nil {
				return err
			}
			csr, err := io.ReadAll(io.LimitReader(c.InOrStdin(), 64<<10))
			if err != nil {
				return fmt.Errorf("origin-cert: reading the CSR: %w", err)
			}
			cert, err := origincert.Issue(c.Context(), api, args[0], csr, days)
			if err != nil {
				return err
			}
			if err := writeOut(c, out, []byte(cert.Certificate)); err != nil {
				return err
			}
			_, err = fmt.Fprintf(c.ErrOrStderr(), "issued %s for %v, expires %s\n", cert.ID, cert.Hostnames, cert.ExpiresOn.Format(time.DateOnly))
			return err
		},
	}
	issue.Flags().StringVarP(&out, "output", "o", "", "file to write the certificate to; standard output when empty")
	issue.Flags().IntVar(&days, "days", 0, "validity in days: 7, 30, 90, 365, 730, 1095 or 5475 (default 5475, fifteen years)")

	var sshKey string
	install := &cobra.Command{
		Use:   "install <zone> <host>",
		Short: "Make a key and CSR on the host, have Cloudflare sign it, and install both for Caddy",
		Long: "install makes the zone's private key and a CSR on the fleet host with openssl, has Cloudflare sign the\n" +
			"CSR for the zone and *.zone, and installs the certificate beside the key as " + caddy.DefaultCertDir + "/<zone>.pem\n" +
			"and .key, root:caddy, mode 0640. The key never leaves the host. Reload Caddy to serve it, and revoke a\n" +
			"certificate it replaces once nothing serves it.",
		Args: cobra.ExactArgs(2),
		RunE: func(c *cobra.Command, args []string) (err error) {
			zone := args[0]
			if !zoneName.MatchString(zone) {
				return fmt.Errorf("origin-cert: %q is not a zone name", zone)
			}
			f, err := load()
			if err != nil {
				return err
			}
			h, ok := f.Host(args[1])
			if !ok {
				return fmt.Errorf("origin-cert: no host %q", args[1])
			}
			api, err := openAPI()
			if err != nil {
				return err
			}
			tr, err := dial(sshKey)(h)
			if err != nil {
				return err
			}
			ctx := c.Context()
			var log bytes.Buffer
			sudo := func(stdout io.Writer, argv ...string) error { return tr.Sudo(ctx, argv, nil, stdout, &log) }
			dir := caddy.DefaultCertDir
			newKey := dir + "/" + zone + ".key.new"
			if err := sudo(&log, "install", "-d", "-o", "root", "-g", "caddy", "-m", "0750", dir); err != nil {
				return fmt.Errorf("origin-cert: %s on %s: %w\n%s", dir, h.Name, err, log.String())
			}
			var csr bytes.Buffer
			if err := sudo(&csr, "openssl", "req", "-new", "-newkey", "rsa:2048", "-noenc", "-batch", "-keyout", newKey); err != nil {
				return fmt.Errorf("origin-cert: making the key and CSR on %s: %w\n%s", h.Name, err, log.String())
			}
			// Whatever happens next, the new key goes if it was not installed.
			installed := false
			defer func() {
				if !installed {
					if rerr := sudo(&log, "rm", "-f", newKey); rerr != nil {
						err = errors.Join(err, fmt.Errorf("origin-cert: removing %s on %s: %w", newKey, h.Name, rerr))
					}
				}
			}()
			cert, err := origincert.Issue(ctx, api, zone, csr.Bytes(), 0)
			if err != nil {
				return err
			}
			local, err := os.CreateTemp("", "deployforge-origin-*.pem")
			if err != nil {
				return err
			}
			defer func() {
				if rerr := os.Remove(local.Name()); rerr != nil {
					err = errors.Join(err, fmt.Errorf("origin-cert: removing the local copy: %w", rerr))
				}
			}()
			if _, err := local.WriteString(cert.Certificate); err != nil {
				return fmt.Errorf("origin-cert: %w", err)
			}
			if err := local.Close(); err != nil {
				return fmt.Errorf("origin-cert: %w", err)
			}
			work, err := tr.TempDir(ctx)
			if err != nil {
				return fmt.Errorf("origin-cert: temporary directory on %s: %w", h.Name, err)
			}
			defer func() {
				if rerr := sudo(&log, "rm", "-rf", work); rerr != nil {
					err = errors.Join(err, fmt.Errorf("origin-cert: removing %s on %s: %w", work, h.Name, rerr))
				}
			}()
			remote := work + "/" + zone + ".pem"
			if err := tr.Upload(ctx, local.Name(), remote); err != nil {
				return fmt.Errorf("origin-cert: %w", err)
			}
			for _, step := range [][]string{
				{"install", "-o", "root", "-g", "caddy", "-m", "0640", remote, dir + "/" + zone + ".pem"},
				{"install", "-o", "root", "-g", "caddy", "-m", "0640", newKey, dir + "/" + zone + ".key"},
			} {
				if err := sudo(&log, step...); err != nil {
					return fmt.Errorf("origin-cert: installing on %s: %w\n%s", h.Name, err, log.String())
				}
			}
			installed = true
			if err := sudo(&log, "rm", "-f", newKey); err != nil {
				return fmt.Errorf("origin-cert: removing %s on %s: %w", newKey, h.Name, err)
			}
			_, err = fmt.Fprintf(c.OutOrStdout(), "%s: installed certificate %s for %v, expires %s; reload Caddy to serve it\n",
				h.Name, cert.ID, cert.Hostnames, cert.ExpiresOn.Format(time.DateOnly))
			return err
		},
	}
	install.Flags().StringVar(&sshKey, "ssh-key", os.Getenv(EnvSSHKey), "the host's SSH private key, used through the agent SSH_AUTH_SOCK names")

	list := &cobra.Command{
		Use:   "list <zone>",
		Short: "List the zone's origin certificates: ID, expiry and hostnames",
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			api, err := openAPI()
			if err != nil {
				return err
			}
			certs, err := api.List(c.Context(), args[0])
			if err != nil {
				return err
			}
			for _, oc := range certs {
				if _, err := fmt.Fprintf(c.OutOrStdout(), "%s  %s  %v\n", oc.ID, oc.ExpiresOn.Format(time.DateOnly), oc.Hostnames); err != nil {
					return err
				}
			}
			return nil
		},
	}
	var confirm bool
	revoke := &cobra.Command{
		Use:   "revoke <id>",
		Short: "Revoke an origin certificate; needs --confirm, as Cloudflare then refuses an origin still serving it",
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			if !confirm {
				return errors.New("origin-cert: revoking cannot be undone, and an origin still serving the certificate is refused; run again with --confirm")
			}
			api, err := openAPI()
			if err != nil {
				return err
			}
			if err := api.Revoke(c.Context(), args[0]); err != nil {
				return err
			}
			_, err = fmt.Fprintf(c.OutOrStdout(), "revoked %s\n", args[0])
			return err
		},
	}
	revoke.Flags().BoolVar(&confirm, "confirm", false, "revoke it")
	root.AddCommand(issue, install, list, revoke)
	return root
}

// originAPI is Cloudflare's Origin CA with the token in TokenEnv.
func originAPI() (origincert.API, error) {
	token := os.Getenv(TokenEnv)
	if token == "" {
		return nil, fmt.Errorf("origin-cert: set %s to a Cloudflare API token with SSL and Certificates Edit on the zone", TokenEnv)
	}
	return origincert.NewCloudflare(token), nil
}
