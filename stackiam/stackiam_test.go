package stackiam

import (
	"bytes"
	"context"
	"errors"
	"net/url"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	iamtypes "github.com/aws/aws-sdk-go-v2/service/iam/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"
)

const testAccount = "123456789012"

var app = Target{App: "example-app", Stack: "prod", Prefix: "/example/prod/example-app/"}

func TestDocument(t *testing.T) {
	p, err := Document(app, "us-west-2", testAccount)
	if err != nil {
		t.Fatal(err)
	}
	text, err := p.JSON()
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`"arn:aws:ssm:us-west-2:123456789012:parameter/example/prod/example-app"`,
		`"arn:aws:ssm:us-west-2:123456789012:parameter/example/prod/example-app/*"`,
		`"ssm:GetParametersByPath"`, `"ssm:PutParameter"`, `"ssm:DescribeParameters"`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the document lacks %s:\n%s", want, text)
		}
	}
	// Only the listing, which IAM cannot scope, reaches beyond the prefix;
	// nothing deletes, and no key is named.
	for _, s := range p.Statement {
		if strings.Contains(strings.Join(s.Action, " "), "Delete") || strings.Contains(strings.Join(s.Action, " "), "kms:") {
			t.Errorf("statement %s: %v", s.Sid, s.Action)
		}
		if s.Resource[0] == "*" && (len(s.Action) != 1 || s.Action[0] != "ssm:DescribeParameters") {
			t.Errorf("statement %s reaches every resource: %v", s.Sid, s.Action)
		}
	}

	for _, bad := range []Target{
		{App: "example-app", Stack: "prod", Prefix: "/"},
		{App: "example-app", Stack: "prod", Prefix: "/example/"},
		{App: "example-app", Stack: "prod", Prefix: "/example/prod/example-app/*/"},
		{App: "Bad", Stack: "prod", Prefix: "/example/prod/example-app/"},
	} {
		if _, err := Document(bad, "us-west-2", testAccount); err == nil {
			t.Errorf("%+v accepted", bad)
		}
	}
	if _, err := Document(app, "us-west-2", "12"); err == nil {
		t.Error("a short account was accepted")
	}
}

func TestRunCreatesThenVerifies(t *testing.T) {
	f := newFakeIAM()
	ctx := context.Background()
	c := Clients{STS: fakeSTS{}, IAM: f}
	o := Options{Region: "us-west-2"}
	other := Target{App: "other-app", Stack: "prod", Prefix: "/example/prod/other-app/"}

	var out bytes.Buffer
	res, err := Run(ctx, c, Plan, o, []Target{app, other}, &out)
	if err != nil || res.Missing != 6 || f.calls != 0 {
		t.Fatalf("plan: %+v %v, %d changes\n%s", res, err, f.calls, out.String())
	}
	if _, err := Run(ctx, c, Verify, o, []Target{app}, &out); !errors.Is(err, ErrNotInPlace) {
		t.Errorf("verify before apply: %v", err)
	}

	out.Reset()
	if _, err := Run(ctx, c, Apply, o, []Target{app, other}, &out); err != nil {
		t.Fatalf("apply: %v\n%s", err, out.String())
	}
	if f.groups["example-app-prod-operators"].attached != "arn:aws:iam::123456789012:policy/deployforge/ssm-example-app-prod" {
		t.Errorf("attached: %+v", f.groups)
	}
	if !strings.Contains(out.String(), "add-user-to-group --group-name example-app-prod-operators") {
		t.Errorf("no note about members:\n%s", out.String())
	}

	// Again, nothing changes.
	calls := f.calls
	f.groups["example-app-prod-operators"].users = []string{"operator"}
	res, err = Run(ctx, c, Verify, o, []Target{app, other}, &out)
	if err != nil || res.Missing != 0 || f.calls != calls || res.Members["example-app-prod-operators"][0] != "operator" {
		t.Fatalf("verify: %+v %v", res, err)
	}

	// A policy edited by hand is put back as a new default version.
	f.policies["arn:aws:iam::123456789012:policy/deployforge/ssm-example-app-prod"].doc = url.QueryEscape(`{"Version":"2012-10-17","Statement":[]}`)
	if _, err := Run(ctx, c, Verify, o, []Target{app}, &out); !errors.Is(err, ErrNotInPlace) {
		t.Errorf("verify of an edited policy: %v", err)
	}
	if _, err := Run(ctx, c, Apply, o, []Target{app}, &out); err != nil {
		t.Fatalf("apply over an edited policy: %v", err)
	}
	if v := f.policies["arn:aws:iam::123456789012:policy/deployforge/ssm-example-app-prod"].versions; v != 2 {
		t.Errorf("versions: %d", v)
	}

	// The wrong account is refused before anything is read.
	if _, err := Run(ctx, c, Plan, Options{Region: "us-west-2", ExpectAccount: "999999999999"}, []Target{app}, &out); err == nil {
		t.Error("another account was accepted")
	}
}

type fakeSTS struct{}

func (fakeSTS) GetCallerIdentity(context.Context, *sts.GetCallerIdentityInput, ...func(*sts.Options)) (*sts.GetCallerIdentityOutput, error) {
	return &sts.GetCallerIdentityOutput{Account: aws.String(testAccount)}, nil
}

type fakePolicy struct {
	doc      string
	versions int
}

type fakeGroup struct {
	attached string
	users    []string
}

type fakeIAM struct {
	policies map[string]*fakePolicy
	groups   map[string]*fakeGroup
	calls    int // changes made
}

func newFakeIAM() *fakeIAM {
	return &fakeIAM{policies: map[string]*fakePolicy{}, groups: map[string]*fakeGroup{}}
}

func noSuch() error { return &iamtypes.NoSuchEntityException{Message: aws.String("no such entity")} }

func (f *fakeIAM) GetPolicy(_ context.Context, in *iam.GetPolicyInput, _ ...func(*iam.Options)) (*iam.GetPolicyOutput, error) {
	if _, ok := f.policies[*in.PolicyArn]; !ok {
		return nil, noSuch()
	}
	return &iam.GetPolicyOutput{Policy: &iamtypes.Policy{DefaultVersionId: aws.String("v")}}, nil
}

func (f *fakeIAM) GetPolicyVersion(_ context.Context, in *iam.GetPolicyVersionInput, _ ...func(*iam.Options)) (*iam.GetPolicyVersionOutput, error) {
	return &iam.GetPolicyVersionOutput{PolicyVersion: &iamtypes.PolicyVersion{Document: aws.String(f.policies[*in.PolicyArn].doc)}}, nil
}

func (f *fakeIAM) ListPolicyVersions(_ context.Context, in *iam.ListPolicyVersionsInput, _ ...func(*iam.Options)) (*iam.ListPolicyVersionsOutput, error) {
	return &iam.ListPolicyVersionsOutput{Versions: make([]iamtypes.PolicyVersion, f.policies[*in.PolicyArn].versions)}, nil
}

func (f *fakeIAM) CreatePolicy(_ context.Context, in *iam.CreatePolicyInput, _ ...func(*iam.Options)) (*iam.CreatePolicyOutput, error) {
	f.calls++
	f.policies["arn:aws:iam::"+testAccount+":policy"+*in.Path+*in.PolicyName] = &fakePolicy{doc: url.QueryEscape(*in.PolicyDocument), versions: 1}
	return &iam.CreatePolicyOutput{}, nil
}

func (f *fakeIAM) CreatePolicyVersion(_ context.Context, in *iam.CreatePolicyVersionInput, _ ...func(*iam.Options)) (*iam.CreatePolicyVersionOutput, error) {
	f.calls++
	p := f.policies[*in.PolicyArn]
	p.doc, p.versions = url.QueryEscape(*in.PolicyDocument), p.versions+1
	return &iam.CreatePolicyVersionOutput{}, nil
}

func (f *fakeIAM) GetGroup(_ context.Context, in *iam.GetGroupInput, _ ...func(*iam.Options)) (*iam.GetGroupOutput, error) {
	g, ok := f.groups[*in.GroupName]
	if !ok {
		return nil, noSuch()
	}
	out := &iam.GetGroupOutput{}
	for _, u := range g.users {
		out.Users = append(out.Users, iamtypes.User{UserName: aws.String(u)})
	}
	return out, nil
}

func (f *fakeIAM) CreateGroup(_ context.Context, in *iam.CreateGroupInput, _ ...func(*iam.Options)) (*iam.CreateGroupOutput, error) {
	f.calls++
	f.groups[*in.GroupName] = &fakeGroup{}
	return &iam.CreateGroupOutput{}, nil
}

func (f *fakeIAM) ListAttachedGroupPolicies(_ context.Context, in *iam.ListAttachedGroupPoliciesInput, _ ...func(*iam.Options)) (*iam.ListAttachedGroupPoliciesOutput, error) {
	out := &iam.ListAttachedGroupPoliciesOutput{}
	if a := f.groups[*in.GroupName].attached; a != "" {
		out.AttachedPolicies = []iamtypes.AttachedPolicy{{PolicyArn: aws.String(a)}}
	}
	return out, nil
}

func (f *fakeIAM) AttachGroupPolicy(_ context.Context, in *iam.AttachGroupPolicyInput, _ ...func(*iam.Options)) (*iam.AttachGroupPolicyOutput, error) {
	f.calls++
	f.groups[*in.GroupName].attached = *in.PolicyArn
	return &iam.AttachGroupPolicyOutput{}, nil
}

func TestPath(t *testing.T) {
	f := newFakeIAM()
	c := Clients{STS: fakeSTS{}, IAM: f}
	var out bytes.Buffer
	if _, err := Run(context.Background(), c, Apply, Options{Region: "us-west-2", Path: "/fleet/"}, []Target{app}, &out); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.policies["arn:aws:iam::123456789012:policy/fleet/ssm-example-app-prod"]; !ok {
		t.Errorf("the policy is not under /fleet/: %v", f.policies)
	}
	if _, err := Run(context.Background(), c, Plan, Options{Region: "us-west-2", Path: "fleet"}, []Target{app}, &out); err == nil {
		t.Error("a path without slashes was accepted")
	}
}
