package origincert

import (
	"context"
	"fmt"
	"time"

	cf "github.com/cloudflare/cloudflare-go/v7"
	"github.com/cloudflare/cloudflare-go/v7/option"
	"github.com/cloudflare/cloudflare-go/v7/origin_ca_certificates"
	"github.com/cloudflare/cloudflare-go/v7/shared"
	"github.com/cloudflare/cloudflare-go/v7/ssl"
	"github.com/cloudflare/cloudflare-go/v7/zones"
)

// Cloudflare is the API over Cloudflare's official Go client.
type Cloudflare struct {
	client *cf.Client
}

// NewCloudflare uses an API token with SSL and Certificates Edit on the
// zones (Read is enough to list).
func NewCloudflare(token string, opts ...option.RequestOption) *Cloudflare {
	return &Cloudflare{client: cf.NewClient(append([]option.RequestOption{option.WithAPIToken(token)}, opts...)...)}
}

// Create implements API.
func (c *Cloudflare) Create(ctx context.Context, r Request) (*Certificate, error) {
	days := r.ValidityDays
	if days == 0 {
		days = 5475
	}
	typ := shared.CertificateRequestTypeOriginRSA
	if r.KeyType == "ecc" {
		typ = shared.CertificateRequestTypeOriginECC
	}
	res, err := c.client.OriginCACertificates.New(ctx, origin_ca_certificates.OriginCACertificateNewParams{
		Csr:               cf.F(r.CSR),
		Hostnames:         cf.F(r.Hostnames),
		RequestType:       cf.F(typ),
		RequestedValidity: cf.F(ssl.RequestValidity(days)),
	})
	if err != nil {
		return nil, err
	}
	return fromAPI(*res)
}

// List implements API.
func (c *Cloudflare) List(ctx context.Context, zone string) ([]Certificate, error) {
	zoneID, err := c.zoneID(ctx, zone)
	if err != nil {
		return nil, err
	}
	pages := c.client.OriginCACertificates.ListAutoPaging(ctx, origin_ca_certificates.OriginCACertificateListParams{ZoneID: cf.F(zoneID)})
	var out []Certificate
	for pages.Next() {
		cert, err := fromAPI(pages.Current())
		if err != nil {
			return nil, err
		}
		out = append(out, *cert)
	}
	return out, pages.Err()
}

// Revoke implements API.
func (c *Cloudflare) Revoke(ctx context.Context, id string) error {
	_, err := c.client.OriginCACertificates.Delete(ctx, id)
	return err
}

func (c *Cloudflare) zoneID(ctx context.Context, name string) (string, error) {
	pages := c.client.Zones.ListAutoPaging(ctx, zones.ZoneListParams{Name: cf.F(name)})
	for pages.Next() {
		if z := pages.Current(); z.Name == name {
			return z.ID, nil
		}
	}
	if err := pages.Err(); err != nil {
		return "", err
	}
	return "", fmt.Errorf("origincert: the token sees no zone %s", name)
}

func fromAPI(c origin_ca_certificates.OriginCACertificate) (*Certificate, error) {
	out := &Certificate{ID: c.ID, Certificate: c.Certificate, Hostnames: c.Hostnames}
	if c.ExpiresOn != "" {
		// Cloudflare writes the expiry as "2041-11-18 19:30:00 +0000 UTC".
		for _, layout := range []string{"2006-01-02 15:04:05 -0700 MST", time.RFC3339} {
			if t, err := time.Parse(layout, c.ExpiresOn); err == nil {
				out.ExpiresOn = t.UTC()
				break
			}
		}
		if out.ExpiresOn.IsZero() {
			return nil, fmt.Errorf("origincert: certificate %s expires on %q, which is not a time", c.ID, c.ExpiresOn)
		}
	}
	return out, nil
}

var _ API = (*Cloudflare)(nil)
