package cli

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	iamtypes "github.com/aws/aws-sdk-go-v2/service/iam/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/spf13/cobra"

	"github.com/plexusone/deployforge/fleet"
	"github.com/plexusone/deployforge/stackiam"
)

func TestIAMTargets(t *testing.T) {
	f, err := fleet.Load(testFleet)
	if err != nil {
		t.Fatal(err)
	}
	all, err := iamTargets(f, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 || all[0].App != "example-app" || all[0].Stack != "prod" || all[0].Prefix != "/example/prod/example-app/" {
		t.Errorf("every live stack: %+v", all)
	}
	named, err := iamTargets(f, []string{"example-app", "staging"})
	if err != nil || len(named) != 1 || named[0].Prefix != "/example/staging/example-app/" {
		t.Errorf("a named stack: %+v, %v", named, err)
	}
	if _, err := iamTargets(f, []string{"example-app"}); err == nil {
		t.Error("an app without a stack")
	}
	if _, err := iamTargets(f, []string{"nowhere", "prod"}); err == nil {
		t.Error("an unknown app")
	}
}

func TestIAMNeedsARegion(t *testing.T) {
	t.Setenv("AWS_REGION", "")
	if _, err := run(t, "iam", "plan"); err == nil || !strings.Contains(err.Error(), "no region") {
		t.Errorf("no region: %v", err)
	}
}

// emptyIAM is an account with no policies or groups, which counts any
// change asked of it.
type emptyIAM struct {
	stackiam.IAM
	changes int
}

func (*emptyIAM) GetPolicy(context.Context, *iam.GetPolicyInput, ...func(*iam.Options)) (*iam.GetPolicyOutput, error) {
	return nil, &iamtypes.NoSuchEntityException{Message: aws.String("no such entity")}
}

func (*emptyIAM) GetGroup(context.Context, *iam.GetGroupInput, ...func(*iam.Options)) (*iam.GetGroupOutput, error) {
	return nil, &iamtypes.NoSuchEntityException{Message: aws.String("no such entity")}
}

func (e *emptyIAM) CreatePolicy(context.Context, *iam.CreatePolicyInput, ...func(*iam.Options)) (*iam.CreatePolicyOutput, error) {
	e.changes++
	return nil, errors.New("a change was made")
}

func (e *emptyIAM) CreateGroup(context.Context, *iam.CreateGroupInput, ...func(*iam.Options)) (*iam.CreateGroupOutput, error) {
	e.changes++
	return nil, errors.New("a change was made")
}

type accountSTS struct{}

func (accountSTS) GetCallerIdentity(context.Context, *sts.GetCallerIdentityInput, ...func(*sts.Options)) (*sts.GetCallerIdentityOutput, error) {
	return &sts.GetCallerIdentityOutput{Account: aws.String("123456789012")}, nil
}

// TestIAMApplyNeedsConfirm checks that apply without --confirm shows the
// plan, changes nothing, and fails saying how to go on.
func TestIAMApplyNeedsConfirm(t *testing.T) {
	fake := &emptyIAM{}
	cmd := New(Options{Fleet: testFleet, IAM: func(*cobra.Command) (stackiam.Clients, error) {
		return stackiam.Clients{STS: accountSTS{}, IAM: fake}, nil
	}})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"iam", "apply", "example-app", "prod", "--region", "us-west-2"})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "run again with --confirm") {
		t.Errorf("apply without --confirm: %v\n%s", err, out.String())
	}
	if fake.changes != 0 {
		t.Errorf("apply without --confirm made %d changes", fake.changes)
	}
	if !strings.Contains(out.String(), "ssm-example-app-prod") {
		t.Errorf("the plan was not shown:\n%s", out.String())
	}
}

// TestIAMPolicyPrintsTheDocumentOffline checks that policy prints one
// stack's document without opening any AWS client.
func TestIAMPolicyPrintsTheDocumentOffline(t *testing.T) {
	cmd := New(Options{Fleet: testFleet, IAM: func(*cobra.Command) (stackiam.Clients, error) {
		return stackiam.Clients{}, errors.New("policy opened AWS")
	}})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"iam", "policy", "example-app", "prod", "--region", "us-west-2", "--account", "123456789012"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("policy: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "arn:aws:ssm:us-west-2:123456789012:parameter/example/prod/example-app/") {
		t.Errorf("document:\n%s", out.String())
	}
	for name, args := range map[string][]string{
		"no account":  {"iam", "policy", "example-app", "prod", "--region", "us-west-2"},
		"every stack": {"iam", "policy", "--region", "us-west-2", "--account", "123456789012"},
	} {
		if _, err := run(t, args...); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}
