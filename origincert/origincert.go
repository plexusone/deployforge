// Package origincert issues a zone's Cloudflare Origin CA certificate from a
// certificate signing request made on the host that serves it, so the
// private key never leaves the host.
package origincert

import (
	"context"
	"crypto/ecdsa"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"slices"
	"time"
)

// Request is a certificate to issue.
type Request struct {
	// CSR is the PEM certificate signing request.
	CSR string
	// Hostnames are the names the certificate covers.
	Hostnames []string
	// KeyType is rsa or ecc, the CSR's key type.
	KeyType string
	// ValidityDays is one of ValidDays; 0 means 5475, fifteen years.
	ValidityDays int
}

// Certificate is an issued origin certificate.
type Certificate struct {
	ID          string
	Certificate string
	Hostnames   []string
	ExpiresOn   time.Time
}

// API is Cloudflare's Origin CA, as this package uses it.
type API interface {
	// Create has Cloudflare sign a request.
	Create(ctx context.Context, r Request) (*Certificate, error)
	// List returns a zone's certificates, by zone name.
	List(ctx context.Context, zone string) ([]Certificate, error)
	// Revoke revokes a certificate by ID.
	Revoke(ctx context.Context, id string) error
}

// ValidDays are the validities Cloudflare offers, in days.
var ValidDays = []int{7, 30, 90, 365, 730, 1095, 5475}

// Hostnames are the names a zone's certificate covers: the zone and every
// host one label under it.
func Hostnames(zone string) []string { return []string{zone, "*." + zone} }

// KeyType reads a PEM certificate signing request, checks its signature,
// and returns its key's type, "rsa" or "ecc".
func KeyType(csrPEM []byte) (string, error) {
	block, _ := pem.Decode(csrPEM)
	if block == nil || block.Type != "CERTIFICATE REQUEST" {
		return "", errors.New("origincert: the input is not a PEM certificate signing request")
	}
	csr, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil {
		return "", fmt.Errorf("origincert: %w", err)
	}
	if err := csr.CheckSignature(); err != nil {
		return "", fmt.Errorf("origincert: the request's signature: %w", err)
	}
	switch csr.PublicKey.(type) {
	case *rsa.PublicKey:
		return "rsa", nil
	case *ecdsa.PublicKey:
		return "ecc", nil
	default:
		return "", fmt.Errorf("origincert: a %T key is neither RSA nor ECDSA", csr.PublicKey)
	}
}

// Issue has Cloudflare sign the CSR as the zone's origin certificate,
// covering the zone and its wildcard, valid for days (0 for fifteen years).
func Issue(ctx context.Context, api API, zone string, csrPEM []byte, days int) (*Certificate, error) {
	if days != 0 && !slices.Contains(ValidDays, days) {
		return nil, fmt.Errorf("origincert: %d days is not one of %v", days, ValidDays)
	}
	keyType, err := KeyType(csrPEM)
	if err != nil {
		return nil, err
	}
	cert, err := api.Create(ctx, Request{CSR: string(csrPEM), Hostnames: Hostnames(zone), KeyType: keyType, ValidityDays: days})
	if err != nil {
		return nil, fmt.Errorf("origincert: %s: %w", zone, err)
	}
	if block, _ := pem.Decode([]byte(cert.Certificate)); block == nil || block.Type != "CERTIFICATE" {
		return nil, fmt.Errorf("origincert: %s: Cloudflare returned no PEM certificate", zone)
	}
	return cert, nil
}
