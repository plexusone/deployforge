// Package awsssm is a secrets.Store over AWS Systems Manager Parameter
// Store, with the operator's own AWS credentials.
package awsssm

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	"github.com/aws/aws-sdk-go-v2/service/ssm/types"

	"github.com/plexusone/deployforge/deploy/secrets"
)

// SSM reads parameters from AWS Systems Manager Parameter Store with the
// operator's own credentials.
type SSM struct {
	client API
}

// API is the part of the SSM client this store uses, so tests can fake it.
type API interface {
	GetParametersByPath(ctx context.Context, in *ssm.GetParametersByPathInput, opts ...func(*ssm.Options)) (*ssm.GetParametersByPathOutput, error)
	GetParameter(ctx context.Context, in *ssm.GetParameterInput, opts ...func(*ssm.Options)) (*ssm.GetParameterOutput, error)
	PutParameter(ctx context.Context, in *ssm.PutParameterInput, opts ...func(*ssm.Options)) (*ssm.PutParameterOutput, error)
}

// NewWithClient uses the given client, such as one with its own
// configuration or a fake in tests.
func NewWithClient(c API) *SSM { return &SSM{client: c} }

// New uses the AWS SDK's default configuration: AWS_PROFILE, AWS_REGION and
// the shared config and credentials files.
func New(ctx context.Context) (*SSM, error) {
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("awsssm: AWS configuration: %w", err)
	}
	if cfg.Region == "" {
		return nil, errors.New("awsssm: no AWS region; set AWS_REGION")
	}
	return &SSM{client: ssm.NewFromConfig(cfg)}, nil
}

// ByPath implements secrets.Store: every parameter directly under path, decrypted,
// across all pages.
func (s *SSM) ByPath(ctx context.Context, path string) ([]secrets.Param, error) {
	// The API names a hierarchy without its trailing slash.
	query := strings.TrimSuffix(path, "/")
	p := ssm.NewGetParametersByPathPaginator(s.client, &ssm.GetParametersByPathInput{
		Path:           aws.String(query),
		Recursive:      aws.Bool(false),
		WithDecryption: aws.Bool(true),
	})
	var out []secrets.Param
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		for _, prm := range page.Parameters {
			out = append(out, secrets.Param{Name: aws.ToString(prm.Name), Value: aws.ToString(prm.Value)})
		}
	}
	return out, nil
}

var _ secrets.Store = (*SSM)(nil)

// Get returns a parameter's decrypted value, and false when there is none.
func (s *SSM) Get(ctx context.Context, name string) (string, bool, error) {
	out, err := s.client.GetParameter(ctx, &ssm.GetParameterInput{Name: aws.String(name), WithDecryption: aws.Bool(true)})
	var nf *types.ParameterNotFound
	switch {
	case errors.As(err, &nf):
		return "", false, nil
	case err != nil:
		return "", false, err
	}
	return aws.ToString(out.Parameter.Value), true, nil
}

// Put creates a SecureString parameter with the account's default key; it
// never overwrites one.
func (s *SSM) Put(ctx context.Context, name, value string) error {
	_, err := s.client.PutParameter(ctx, &ssm.PutParameterInput{
		Name: aws.String(name), Value: aws.String(value), Type: types.ParameterTypeSecureString, Overwrite: aws.Bool(false),
	})
	if err != nil {
		return fmt.Errorf("put %s: %w", name, err)
	}
	return nil
}

// Set writes a SecureString parameter with the account's default key,
// creating it or replacing its value.
func (s *SSM) Set(ctx context.Context, name, value string) error {
	_, err := s.client.PutParameter(ctx, &ssm.PutParameterInput{
		Name: aws.String(name), Value: aws.String(value), Type: types.ParameterTypeSecureString, Overwrite: aws.Bool(true),
	})
	if err != nil {
		return fmt.Errorf("set %s: %w", name, err)
	}
	return nil
}
