package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/lightsail"
	"github.com/spf13/cobra"

	"github.com/plexusone/deployforge/dbsnapshot"
	"github.com/plexusone/deployforge/fleet"
)

// lightsailAPI is the Lightsail client with the default AWS configuration.
func lightsailAPI(ctx context.Context) (dbsnapshot.API, error) {
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("db snapshot: AWS configuration: %w", err)
	}
	if cfg.Region == "" {
		return nil, errors.New("db snapshot: no AWS region; set AWS_REGION")
	}
	return lightsail.NewFromConfig(cfg), nil
}

// snapshotServer is the fleet's database server's Lightsail name.
func snapshotServer(f *fleet.Fleet) (string, error) {
	if f.DatabaseServer == nil || f.DatabaseServer.LightsailName == "" {
		return "", errors.New("db snapshot: the fleet document's databaseServer has no lightsailName")
	}
	return f.DatabaseServer.LightsailName, nil
}

// takesSnapshot reports whether a deploy of the stack snapshots the server
// first: the primary stack of an app with databases, when the server is a
// Lightsail database.
func takesSnapshot(f *fleet.Fleet, a fleet.App, stack string) bool {
	s := a.Stacks[stack]
	return stack == f.Primary() && len(s.Databases) > 0 && f.DatabaseServer != nil && f.DatabaseServer.LightsailName != ""
}

// snapshotBeforeDeploy takes a snapshot for a deploy and waits for it.
func snapshotBeforeDeploy(ctx context.Context, api dbsnapshot.API, server, app, stack, label string, log io.Writer) (dbsnapshot.Snapshot, error) {
	p, err := dbsnapshot.Prepare(ctx, api, dbsnapshot.Request{Server: server, App: app, Stack: stack, Label: label, Now: time.Now()})
	if err != nil {
		return dbsnapshot.Snapshot{}, err
	}
	return dbsnapshot.Create(ctx, api, p, dbsnapshot.Wait{}, log)
}

func snapshotCmd(load func() (*fleet.Fleet, error), openAPI func(*cobra.Command) (dbsnapshot.API, error)) *cobra.Command {
	root := &cobra.Command{
		Use:   "snapshot",
		Short: "Plan, take, verify and list manual snapshots of the fleet's database server",
		Long: "snapshot takes a manual Lightsail snapshot of the fleet's database server, before a deploy whose migrate\n" +
			"changes a schema, and waits until it is available. A server holds every app's databases, so the snapshot\n" +
			"does too; the app, stack and --label (such as the release tag) name it and are kept as tags. deploy ship\n" +
			"takes one itself before deploying an app's primary stack. Nothing is deleted: remove old snapshots in the\n" +
			"Lightsail console, as each is billed for its storage.",
	}
	resolve := func(c *cobra.Command) (*fleet.Fleet, dbsnapshot.API, string, error) {
		f, err := load()
		if err != nil {
			return nil, nil, "", err
		}
		server, err := snapshotServer(f)
		if err != nil {
			return nil, nil, "", err
		}
		api, err := openAPI(c)
		return f, api, server, err
	}
	var label string
	request := func(c *cobra.Command, args []string) (dbsnapshot.API, dbsnapshot.Plan, error) {
		f, api, server, err := resolve(c)
		if err != nil {
			return nil, dbsnapshot.Plan{}, err
		}
		if _, _, err := f.Stack(args[0], args[1]); err != nil {
			return nil, dbsnapshot.Plan{}, err
		}
		p, err := dbsnapshot.Prepare(c.Context(), api, dbsnapshot.Request{Server: server, App: args[0], Stack: args[1], Label: label, Now: time.Now()})
		return api, p, err
	}

	plan := &cobra.Command{
		Use:   "plan <app> <stack>",
		Short: "Show the server, its state, the snapshot apply would take and the recent ones; change nothing",
		Args:  cobra.ExactArgs(2),
		RunE: func(c *cobra.Command, args []string) error {
			_, p, err := request(c, args)
			if err != nil {
				return err
			}
			w := c.OutOrStdout()
			if _, err := fmt.Fprintf(w, "server:   %s (%s)\nsnapshot: %s\n", p.Server, p.ServerState, p.Name); err != nil {
				return err
			}
			if p.ServerState != dbsnapshot.Available {
				if _, err := fmt.Fprintf(w, "apply would wait: the server must be %s\n", dbsnapshot.Available); err != nil {
					return err
				}
			}
			return printSnapshots(w, p.Recent, 5)
		},
	}
	plan.Flags().StringVar(&label, "label", "", "what the snapshot is for, such as the release tag")

	var confirm bool
	var timeout time.Duration
	apply := &cobra.Command{
		Use:   "apply <app> <stack> --confirm",
		Short: "Take the snapshot and wait until it is available",
		Args:  cobra.ExactArgs(2),
		RunE: func(c *cobra.Command, args []string) error {
			if !confirm {
				return errors.New("db snapshot: apply takes a snapshot, billed for its storage; run plan, then apply --confirm")
			}
			api, p, err := request(c, args)
			if err != nil {
				return err
			}
			s, err := dbsnapshot.Create(c.Context(), api, p, dbsnapshot.Wait{Timeout: timeout}, c.ErrOrStderr())
			if err != nil {
				return err
			}
			_, err = fmt.Fprintf(c.OutOrStdout(), "ok  %s is %s\n", s.Name, s.State)
			return err
		},
	}
	apply.Flags().StringVar(&label, "label", "", "what the snapshot is for, such as the release tag")
	apply.Flags().BoolVar(&confirm, "confirm", false, "take the snapshot")
	apply.Flags().DurationVar(&timeout, "timeout", 30*time.Minute, "how long to wait for the snapshot")

	verify := &cobra.Command{
		Use:   "verify <snapshot>",
		Short: "Exit non-zero unless the snapshot is available",
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			api, err := openAPI(c)
			if err != nil {
				return err
			}
			s, err := dbsnapshot.Verify(c.Context(), api, args[0])
			if err != nil {
				return err
			}
			_, err = fmt.Fprintf(c.OutOrStdout(), "ok  %s of %s is %s, %d GB, taken %s\n", s.Name, s.Server, s.State, s.SizeGB, s.Created.UTC().Format(time.RFC3339))
			return err
		},
	}

	list := &cobra.Command{
		Use:   "list",
		Short: "List the server's manual snapshots, newest first",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			_, api, server, err := resolve(c)
			if err != nil {
				return err
			}
			all, err := dbsnapshot.List(c.Context(), api, server)
			if err != nil {
				return err
			}
			if _, err := fmt.Fprintf(c.OutOrStdout(), "server: %s\n", server); err != nil {
				return err
			}
			return printSnapshots(c.OutOrStdout(), all, 0)
		},
	}
	root.AddCommand(plan, apply, verify, list)
	return root
}

func printSnapshots(w io.Writer, all []dbsnapshot.Snapshot, limit int) error {
	if len(all) == 0 {
		_, err := fmt.Fprintln(w, "no manual snapshots")
		return err
	}
	if _, err := fmt.Fprintf(w, "manual snapshots (%d), newest first:\n", len(all)); err != nil {
		return err
	}
	for i, s := range all {
		if limit > 0 && i == limit {
			_, err := fmt.Fprintf(w, "  … and %d more; see db snapshot list\n", len(all)-limit)
			return err
		}
		if _, err := fmt.Fprintf(w, "  %s  %-10s %3d GB  %s\n", s.Created.UTC().Format("2006-01-02 15:04Z"), s.State, s.SizeGB, s.Name); err != nil {
			return err
		}
	}
	return nil
}
