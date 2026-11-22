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
	t         *testing.T
	calls     int
	overwrite bool
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

func (f *fake) GetParameter(_ context.Context, in *ssm.GetParameterInput, _ ...func(*ssm.Options)) (*ssm.GetParameterOutput, error) {
	if aws.ToString(in.Name) == "/missing" {
		return nil, &types.ParameterNotFound{}
	}
	return &ssm.GetParameterOutput{Parameter: &types.Parameter{Name: in.Name, Value: aws.String("v")}}, nil
}

func (f *fake) PutParameter(_ context.Context, in *ssm.PutParameterInput, _ ...func(*ssm.Options)) (*ssm.PutParameterOutput, error) {
	if in.Type != types.ParameterTypeSecureString {
		f.t.Errorf("put %+v", in)
	}
	f.overwrite = aws.ToBool(in.Overwrite)
	f.calls++
	return &ssm.PutParameterOutput{}, nil
}

func TestGetAndPut(t *testing.T) {
	f := &fake{t: t}
	s := NewWithClient(f)
	if v, ok, err := s.Get(context.Background(), "/a"); err != nil || !ok || v != "v" {
		t.Errorf("Get = %q, %v, %v", v, ok, err)
	}
	if _, ok, err := s.Get(context.Background(), "/missing"); err != nil || ok {
		t.Errorf("a missing parameter: %v, %v", ok, err)
	}
	if err := s.Put(context.Background(), "/b", "x"); err != nil || f.calls != 1 || f.overwrite {
		t.Errorf("Put: %v, overwrite %v", err, f.overwrite)
	}
	if err := s.Set(context.Background(), "/b", "y"); err != nil || f.calls != 2 || !f.overwrite {
		t.Errorf("Set: %v, overwrite %v", err, f.overwrite)
	}
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
