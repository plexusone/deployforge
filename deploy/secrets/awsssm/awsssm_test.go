package awsssm

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	"github.com/aws/aws-sdk-go-v2/service/ssm/types"
)

// fake serves two pages and checks the query.
type fake struct {
	t     *testing.T
	calls int
}

func (f *fake) GetParametersByPath(_ context.Context, in *ssm.GetParametersByPathInput, _ ...func(*ssm.Options)) (*ssm.GetParametersByPathOutput, error) {
	f.calls++
	if aws.ToString(in.Path) != "/example/prod/app" || aws.ToBool(in.Recursive) || !aws.ToBool(in.WithDecryption) {
		f.t.Errorf("query %+v", in)
	}
	if in.NextToken == nil {
		return &ssm.GetParametersByPathOutput{
			Parameters: []types.Parameter{{Name: aws.String("/example/prod/app/A"), Value: aws.String("1")}},
			NextToken:  aws.String("next"),
		}, nil
	}
	return &ssm.GetParametersByPathOutput{
		Parameters: []types.Parameter{{Name: aws.String("/example/prod/app/B"), Value: aws.String("2")}},
	}, nil
}

func TestByPathReadsEveryPage(t *testing.T) {
	f := &fake{t: t}
	got, err := NewWithClient(f).ByPath(context.Background(), "/example/prod/app/")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Name != "/example/prod/app/A" || got[1].Value != "2" || f.calls != 2 {
		t.Errorf("got %+v after %d calls", got, f.calls)
	}
}
