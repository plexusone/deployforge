// Package stackiam gives each app stack's parameter-store prefix its own
// IAM policy and operators' group, so the operators of one app stack can
// read and write that stack's parameters and no other's, when deploys read
// the parameter store with the operator's own identity.
//
// Each stack gets a customer-managed policy, ssm-<app>-<stack>, that allows
// reading and writing parameters under the prefix and listing parameter
// names, which credential checks use, and a group, <app>-<stack>-operators,
// with the policy attached. Membership is the operator's choice and is
// reported, never changed. The parameters are SecureStrings under SSM's
// AWS-managed key, whose key policy already lets anyone the account allows
// to use SSM decrypt through it, so the policy names no key.
//
// Plan says what is missing or out of date; apply creates the policy and
// group, attaches one to the other, and sets an out-of-date policy's default
// version; verify fails unless everything is in place. Nothing is deleted.
package stackiam

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"reflect"
	"regexp"
	"slices"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	iamtypes "github.com/aws/aws-sdk-go-v2/service/iam/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"
)

// Mode is what a run does.
type Mode string

// The modes.
const (
	// Plan says what is missing and what apply would do; it changes nothing.
	Plan Mode = "plan"
	// Apply creates or updates what is missing, then verifies.
	Apply Mode = "apply"
	// Verify checks that everything is in place.
	Verify Mode = "verify"
)

// Modes are the valid modes.
var Modes = []Mode{Plan, Apply, Verify}

// DefaultPath is the IAM path of the policies and groups, by which they are
// told apart from anything else in the account, when Options.Path is empty.
const DefaultPath = "/deployforge/"

// maxVersions is IAM's limit on a managed policy's versions.
const maxVersions = 5

// Target is one app stack and its parameter-store prefix.
type Target struct {
	App, Stack string
	// Prefix is the stack's SSM prefix, such as /example/prod/example-app/.
	Prefix string
}

// PolicyName is the stack's policy: ssm-<app>-<stack>.
func (t Target) PolicyName() string { return "ssm-" + t.App + "-" + t.Stack }

// GroupName is the stack's operators' group: <app>-<stack>-operators.
func (t Target) GroupName() string { return t.App + "-" + t.Stack + "-operators" }

var (
	namePattern   = regexp.MustCompile(`^[a-z][a-z0-9-]*[a-z0-9]$`)
	prefixPattern = regexp.MustCompile(`^(/[a-z0-9_.-]+)+/$`)
	regionPattern = regexp.MustCompile(`^[a-z]{2}(-[a-z]+)+-[0-9]$`)
	account12     = regexp.MustCompile(`^[0-9]{12}$`)
	pathPattern   = regexp.MustCompile(`^/([A-Za-z0-9_+=,.@-]+/)*$`)
)

func (t Target) check() error {
	switch {
	case !namePattern.MatchString(t.App) || !namePattern.MatchString(t.Stack):
		return fmt.Errorf("stackiam: %q %q is not an app and stack", t.App, t.Stack)
	case !prefixPattern.MatchString(t.Prefix) || strings.Count(t.Prefix, "/") < 3:
		return fmt.Errorf("stackiam: %q is not an SSM prefix of at least two levels, such as /org/prod/app/", t.Prefix)
	}
	return nil
}

// Policy is an IAM policy document.
type Policy struct {
	Version   string      `json:"Version"`
	Statement []Statement `json:"Statement"`
}

// Statement is one statement of a policy.
type Statement struct {
	Sid      string   `json:"Sid"`
	Effect   string   `json:"Effect"`
	Action   []string `json:"Action"`
	Resource []string `json:"Resource"`
}

// Document is the stack's policy in region and account: read and write the
// parameters under the prefix, and list parameter names, which IAM cannot
// limit to a path.
func Document(t Target, region, account string) (Policy, error) {
	if err := t.check(); err != nil {
		return Policy{}, err
	}
	if !regionPattern.MatchString(region) || !account12.MatchString(account) {
		return Policy{}, fmt.Errorf("stackiam: %q %q is not a region and account", region, account)
	}
	arn := "arn:aws:ssm:" + region + ":" + account + ":parameter" + strings.TrimSuffix(t.Prefix, "/")
	return Policy{Version: "2012-10-17", Statement: []Statement{
		{
			Sid: "StackParameters", Effect: "Allow",
			Action: []string{"ssm:GetParameter", "ssm:GetParameters", "ssm:GetParametersByPath", "ssm:PutParameter"},
			// The path itself for GetParametersByPath, and everything under it.
			Resource: []string{arn, arn + "/*"},
		},
		{
			Sid: "ParameterNames", Effect: "Allow",
			Action:   []string{"ssm:DescribeParameters"},
			Resource: []string{"*"},
		},
	}}, nil
}

// JSON is the policy as IAM takes it, indented.
func (p Policy) JSON() (string, error) {
	b, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return "", fmt.Errorf("stackiam: %w", err)
	}
	return string(b), nil
}

// STS is the part of the STS client a run uses.
type STS interface {
	GetCallerIdentity(ctx context.Context, in *sts.GetCallerIdentityInput, opts ...func(*sts.Options)) (*sts.GetCallerIdentityOutput, error)
}

// IAM is the part of the IAM client a run uses.
type IAM interface {
	GetPolicy(ctx context.Context, in *iam.GetPolicyInput, opts ...func(*iam.Options)) (*iam.GetPolicyOutput, error)
	GetPolicyVersion(ctx context.Context, in *iam.GetPolicyVersionInput, opts ...func(*iam.Options)) (*iam.GetPolicyVersionOutput, error)
	ListPolicyVersions(ctx context.Context, in *iam.ListPolicyVersionsInput, opts ...func(*iam.Options)) (*iam.ListPolicyVersionsOutput, error)
	CreatePolicy(ctx context.Context, in *iam.CreatePolicyInput, opts ...func(*iam.Options)) (*iam.CreatePolicyOutput, error)
	CreatePolicyVersion(ctx context.Context, in *iam.CreatePolicyVersionInput, opts ...func(*iam.Options)) (*iam.CreatePolicyVersionOutput, error)
	GetGroup(ctx context.Context, in *iam.GetGroupInput, opts ...func(*iam.Options)) (*iam.GetGroupOutput, error)
	CreateGroup(ctx context.Context, in *iam.CreateGroupInput, opts ...func(*iam.Options)) (*iam.CreateGroupOutput, error)
	ListAttachedGroupPolicies(ctx context.Context, in *iam.ListAttachedGroupPoliciesInput, opts ...func(*iam.Options)) (*iam.ListAttachedGroupPoliciesOutput, error)
	AttachGroupPolicy(ctx context.Context, in *iam.AttachGroupPolicyInput, opts ...func(*iam.Options)) (*iam.AttachGroupPolicyOutput, error)
}

// Clients are the AWS clients.
type Clients struct {
	STS STS
	IAM IAM
}

// Options name the region and, optionally, the only account to act in and
// the IAM path.
type Options struct {
	Region        string
	ExpectAccount string
	// Path is the IAM path, such as /deployforge/; DefaultPath when empty.
	Path string
}

// Result is what a run found.
type Result struct {
	Account string
	// Missing counts the items found missing or out of date, or that apply
	// could not confirm.
	Missing int
	// Members are each group's users, by group name.
	Members map[string][]string
}

// ErrNotInPlace is returned by verify, and by apply when its final check
// fails, when any item is missing or out of date.
var ErrNotInPlace = errors.New("stackiam: not everything is in place")

// Run checks the account, then plans, applies or verifies each target's
// policy and group, reporting each item to out.
func Run(ctx context.Context, c Clients, mode Mode, o Options, targets []Target, out io.Writer) (Result, error) {
	if !slices.Contains(Modes, mode) {
		return Result{}, fmt.Errorf("stackiam: mode %q is not one of %v", mode, Modes)
	}
	if len(targets) == 0 {
		return Result{}, errors.New("stackiam: no stacks")
	}
	id, err := c.STS.GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err != nil {
		return Result{}, fmt.Errorf("stackiam: cannot identify the AWS account; is the profile valid? %w", err)
	}
	account := aws.ToString(id.Account)
	if o.ExpectAccount != "" && account != o.ExpectAccount {
		return Result{}, fmt.Errorf("stackiam: the profile is account %s, not the expected %s", account, o.ExpectAccount)
	}
	docs := make([]Policy, len(targets))
	for i, t := range targets {
		if docs[i], err = Document(t, o.Region, account); err != nil {
			return Result{}, err
		}
	}
	iamPath := o.Path
	if iamPath == "" {
		iamPath = DefaultPath
	}
	if !pathPattern.MatchString(iamPath) {
		return Result{}, fmt.Errorf("stackiam: IAM path %q must start and end with /", iamPath)
	}
	r := &run{c: c, account: account, out: out, mode: mode, path: iamPath, members: map[string][]string{}}
	r.say("account %s, region %s, mode %s", account, o.Region, mode)
	all := func() error {
		for i, t := range targets {
			if err := r.target(ctx, t, docs[i]); err != nil {
				return err
			}
		}
		return nil
	}
	if err := all(); err != nil {
		return Result{}, err
	}
	if mode == Apply {
		// Check again, so apply never reports what it did not confirm.
		r.mode, r.missing = Verify, 0
		if err := all(); err != nil {
			return Result{}, err
		}
		r.mode = Apply
	}
	res := Result{Account: account, Missing: r.missing, Members: r.members}
	if r.missing > 0 {
		r.say("%d item(s) need attention", r.missing)
		if mode == Plan {
			return res, r.err
		}
		return res, ErrNotInPlace
	}
	r.say("everything is in place")
	return res, r.err
}

type run struct {
	c       Clients
	account string
	out     io.Writer
	mode    Mode
	path    string
	missing int
	members map[string][]string
	err     error
}

func (r *run) say(format string, a ...any) {
	if r.err == nil {
		_, r.err = fmt.Fprintf(r.out, format+"\n", a...)
	}
}

// report says how an item stands; ok is false when it is missing.
func (r *run) report(ok bool, item, fix string) {
	switch {
	case ok:
		r.say("  ok       %s", item)
	case r.mode == Apply:
		r.say("  apply    %s: %s", item, fix)
	default:
		r.missing++
		r.say("  missing  %s: apply would %s", item, fix)
	}
}

func (r *run) target(ctx context.Context, t Target, doc Policy) error {
	r.say("%s %s (%s)", t.App, t.Stack, t.Prefix)
	policyARN := "arn:aws:iam::" + r.account + ":policy" + r.path + t.PolicyName()
	if err := r.policy(ctx, t, doc, policyARN); err != nil {
		return err
	}
	return r.group(ctx, t, policyARN)
}

func (r *run) policy(ctx context.Context, t Target, doc Policy, policyARN string) error {
	text, err := doc.JSON()
	if err != nil {
		return err
	}
	item := "policy " + t.PolicyName()
	got, err := r.c.IAM.GetPolicy(ctx, &iam.GetPolicyInput{PolicyArn: aws.String(policyARN)})
	var nf *iamtypes.NoSuchEntityException
	switch {
	case errors.As(err, &nf):
		r.report(false, item, "create it")
		if r.mode == Apply {
			if _, err := r.c.IAM.CreatePolicy(ctx, &iam.CreatePolicyInput{
				PolicyName: aws.String(t.PolicyName()), Path: aws.String(r.path), PolicyDocument: aws.String(text),
				Description: aws.String("Read and write SSM parameters under " + t.Prefix + ", and list parameter names"),
			}); err != nil {
				return fmt.Errorf("stackiam: creating %s: %w", item, err)
			}
		}
		return nil
	case err != nil:
		return fmt.Errorf("stackiam: reading %s: %w", item, err)
	}
	v, err := r.c.IAM.GetPolicyVersion(ctx, &iam.GetPolicyVersionInput{PolicyArn: aws.String(policyARN), VersionId: got.Policy.DefaultVersionId})
	if err != nil {
		return fmt.Errorf("stackiam: reading %s's document: %w", item, err)
	}
	same, err := sameDocument(aws.ToString(v.PolicyVersion.Document), doc)
	if err != nil {
		return fmt.Errorf("stackiam: %s: %w", item, err)
	}
	if same {
		r.report(true, item, "")
		return nil
	}
	versions, err := r.c.IAM.ListPolicyVersions(ctx, &iam.ListPolicyVersionsInput{PolicyArn: aws.String(policyARN)})
	if err != nil {
		return fmt.Errorf("stackiam: listing %s's versions: %w", item, err)
	}
	if len(versions.Versions) >= maxVersions {
		r.missing++
		r.say("  missing  %s: its document is out of date and it has IAM's %d versions; delete an old version by hand, then apply", item, maxVersions)
		return nil
	}
	r.report(false, item+" document", "make the current document its default version")
	if r.mode == Apply {
		if _, err := r.c.IAM.CreatePolicyVersion(ctx, &iam.CreatePolicyVersionInput{
			PolicyArn: aws.String(policyARN), PolicyDocument: aws.String(text), SetAsDefault: true,
		}); err != nil {
			return fmt.Errorf("stackiam: updating %s: %w", item, err)
		}
	}
	return nil
}

func (r *run) group(ctx context.Context, t Target, policyARN string) error {
	item := "group " + t.GroupName()
	g, err := r.c.IAM.GetGroup(ctx, &iam.GetGroupInput{GroupName: aws.String(t.GroupName())})
	var nf *iamtypes.NoSuchEntityException
	exists := true
	switch {
	case errors.As(err, &nf):
		exists = false
		r.report(false, item, "create it")
		if r.mode == Apply {
			if _, err := r.c.IAM.CreateGroup(ctx, &iam.CreateGroupInput{GroupName: aws.String(t.GroupName()), Path: aws.String(r.path)}); err != nil {
				return fmt.Errorf("stackiam: creating %s: %w", item, err)
			}
		}
	case err != nil:
		return fmt.Errorf("stackiam: reading %s: %w", item, err)
	default:
		r.report(true, item, "")
	}
	attached := false
	if exists {
		a, err := r.c.IAM.ListAttachedGroupPolicies(ctx, &iam.ListAttachedGroupPoliciesInput{GroupName: aws.String(t.GroupName())})
		if err != nil {
			return fmt.Errorf("stackiam: listing %s's policies: %w", item, err)
		}
		for _, p := range a.AttachedPolicies {
			attached = attached || aws.ToString(p.PolicyArn) == policyARN
		}
	}
	r.report(attached, item+" has "+t.PolicyName(), "attach it")
	if !attached && r.mode == Apply {
		if _, err := r.c.IAM.AttachGroupPolicy(ctx, &iam.AttachGroupPolicyInput{GroupName: aws.String(t.GroupName()), PolicyArn: aws.String(policyARN)}); err != nil {
			return fmt.Errorf("stackiam: attaching %s to %s: %w", t.PolicyName(), item, err)
		}
	}
	var users []string
	if exists {
		for _, u := range g.Users {
			users = append(users, aws.ToString(u.UserName))
		}
	}
	r.members[t.GroupName()] = users
	if len(users) == 0 {
		r.say("  note     %s has no members: add the stack's operators with aws iam add-user-to-group --group-name %s --user-name <user>", item, t.GroupName())
	} else {
		r.say("  members  %s", strings.Join(users, ", "))
	}
	return nil
}

// sameDocument reports whether IAM's document, which it returns
// URL-encoded, is want.
func sameDocument(encoded string, want Policy) (bool, error) {
	text, err := url.QueryUnescape(encoded)
	if err != nil {
		return false, fmt.Errorf("decoding the policy document: %w", err)
	}
	var got, w any
	if err := json.Unmarshal([]byte(text), &got); err != nil {
		return false, fmt.Errorf("parsing the policy document: %w", err)
	}
	b, err := json.Marshal(want)
	if err != nil {
		return false, err
	}
	if err := json.Unmarshal(b, &w); err != nil {
		return false, err
	}
	return reflect.DeepEqual(got, w), nil
}
