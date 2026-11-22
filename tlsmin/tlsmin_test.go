package tlsmin

import (
	"context"
	"crypto/tls"
	"errors"
	"strings"
	"testing"
)

// server accepts versions from min up.
func server(min uint16) Dial {
	return func(_ context.Context, _ string, max uint16) error {
		if max < min {
			return errors.New("protocol version not supported")
		}
		return nil
	}
}

func TestCheck(t *testing.T) {
	ctx := context.Background()
	if err := Check(ctx, "app.example.com", server(tls.VersionTLS12)); err != nil {
		t.Errorf("a 1.2 minimum: %v", err)
	}
	if err := Check(ctx, "app.example.com", server(tls.VersionTLS10)); err == nil || !strings.Contains(err.Error(), "accepts TLS 1.1") {
		t.Errorf("a 1.0 minimum: %v", err)
	}
	if err := Check(ctx, "app.example.com", server(tls.VersionTLS13)); err == nil || !strings.Contains(err.Error(), "refuses TLS 1.2") {
		t.Errorf("a 1.3 minimum: %v", err)
	}
}
