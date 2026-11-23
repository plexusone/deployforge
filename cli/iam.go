package cli

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/spf13/cobra"

	"github.com/plexusone/deployforge/fleet"
	"github.com/plexusone/deployforge/stackiam"
)

// iamClients connects to IAM and STS with the default AWS configuration.
func iamClients(ctx context.Context) (stackiam.Clients, error) {
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return stackiam.Clients{}, fmt.Errorf("iam: AWS configuration: %w", err)
	}
	return stackiam.Clients{STS: sts.NewFromConfig(cfg), IAM: iam.NewFromConfig(cfg)}, nil
}

// iamTargets are the named stacks, or every live stack, with their
// prefixes.
func iamTargets(f *fleet.Fleet, args []string) ([]stackiam.Target, error) {
	if len(args)%2 != 0 {
		return nil, errors.New("iam: give stacks as <app> <stack> pairs")
	}
	var out []stackiam.Target
	add := func(a fleet.App, stack string) {
		out = append(out, stackiam.Target{App: a.Name, Stack: stack, Prefix: a.SecretPrefixFor(stack)})
	}
	if len(args) == 0 {
		for _, a := range f.Apps {
			for _, stack := range f.StackNames(a) {
				if a.Stacks[stack].Status == fleet.Live {
					add(a, stack)
				}
			}
		}
		return out, nil
	}
	// The count is even, so each app has its stack; i+1 < len says so to
	// the compiler and to gosec.
	for i := 0; i+1 < len(args); i += 2 {
		a, _, err := f.Stack(args[i], args[i+1])
		if err != nil {
			return nil, err
		}
		add(a, args[i+1])
	}
	return out, nil
}

func iamCmd(load func() (*fleet.Fleet, error), open func(*cobra.Command) (stackiam.Clients, error)) *cobra.Command {
	var region, account, path string
	cmd := &cobra.Command{
		Use:   "iam <plan|apply|verify> [<app> <stack>]...",
		Short: "Give each app stack's parameter-store prefix its own IAM policy and operators' group",
		Long: "iam makes, for each named stack or every live one, a policy ssm-<app>-<stack> that reads and writes\n" +
			"the SSM parameters under the stack's secret prefix and lists parameter names, and a group\n" +
			"<app>-<stack>-operators with the policy attached. Group members are reported, never changed.\n" +
			"plan lists what is missing, apply creates it and checks again, verify fails unless all is in place.\n" +
			"Nothing is deleted.",
		Args:      cobra.MinimumNArgs(1),
		ValidArgs: []string{"plan", "apply", "verify"},
		RunE: func(c *cobra.Command, args []string) error {
			f, err := load()
			if err != nil {
				return err
			}
			targets, err := iamTargets(f, args[1:])
			if err != nil {
				return err
			}
			if region == "" {
				return errors.New("iam: no region; pass --region or set AWS_REGION")
			}
			clients, err := open(c)
			if err != nil {
				return err
			}
			_, err = stackiam.Run(c.Context(), clients, stackiam.Mode(args[0]), stackiam.Options{Region: region, ExpectAccount: account, Path: path}, targets, c.OutOrStdout())
			return err
		},
	}
	fl := cmd.Flags()
	fl.StringVar(&region, "region", os.Getenv("AWS_REGION"), "the region of the parameters; default $AWS_REGION")
	fl.StringVar(&account, "account", "", "refuse to act unless the AWS profile is this account")
	fl.StringVar(&path, "path", stackiam.DefaultPath, "the IAM path of the policies and groups")
	return cmd
}
