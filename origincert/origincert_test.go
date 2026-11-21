package origincert

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"slices"
	"strings"
	"testing"

	"github.com/cloudflare/cloudflare-go/v7/origin_ca_certificates"
)

func csr(t *testing.T, key any) []byte {
	t.Helper()
	der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{}, key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der})
}

type fakeAPI struct{ got Request }

func (f *fakeAPI) Create(_ context.Context, r Request) (*Certificate, error) {
	f.got = r
	return &Certificate{ID: "1", Certificate: "-----BEGIN CERTIFICATE-----\nAA==\n-----END CERTIFICATE-----\n", Hostnames: r.Hostnames}, nil
}
func (f *fakeAPI) List(context.Context, string) ([]Certificate, error) { return nil, nil }
func (f *fakeAPI) Revoke(context.Context, string) error                { return nil }

func TestIssueSignsForTheZoneAndItsWildcard(t *testing.T) {
	rk, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	ek, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	for want, key := range map[string]any{"rsa": rk, "ecc": ek} {
		api := &fakeAPI{}
		if _, err := Issue(context.Background(), api, "example.com", csr(t, key), 0); err != nil {
			t.Fatal(err)
		}
		if api.got.KeyType != want || !slices.Equal(api.got.Hostnames, []string{"example.com", "*.example.com"}) {
			t.Errorf("%s: request %+v", want, api.got)
		}
	}
	if _, err := Issue(context.Background(), &fakeAPI{}, "example.com", csr(t, ek), 42); err == nil || !strings.Contains(err.Error(), "42 days") {
		t.Errorf("an unoffered validity: %v", err)
	}
	if _, err := Issue(context.Background(), &fakeAPI{}, "example.com", []byte("not a csr"), 0); err == nil {
		t.Error("a request that is not a CSR")
	}
}

func TestFromAPIReadsCloudflaresExpiry(t *testing.T) {
	c, err := fromAPI(origin_ca_certificates.OriginCACertificate{ID: "1", ExpiresOn: "2041-11-18 19:30:00 +0000 UTC"})
	if err != nil || c.ExpiresOn.Year() != 2041 {
		t.Errorf("%+v, %v", c, err)
	}
	if _, err := fromAPI(origin_ca_certificates.OriginCACertificate{ID: "1", ExpiresOn: "soon"}); err == nil {
		t.Error("an expiry that is not a time")
	}
}
