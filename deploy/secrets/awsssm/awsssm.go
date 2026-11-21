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
