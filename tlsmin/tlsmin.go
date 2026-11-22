// Package tlsmin checks from outside that a host refuses TLS versions
// before 1.2 and accepts TLS 1.2, as a CDN's or a server's minimum TLS
// version setting should make it.
package tlsmin

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"time"
)

// Timeout is how long each handshake may take.
const Timeout = 10 * time.Second

// Dial opens a TLS connection to host:443 offering versions up to max.
type Dial func(ctx context.Context, host string, max uint16) error

// Direct dials host:443 over the network.
func Direct(ctx context.Context, host string, max uint16) error {
	d := &tls.Dialer{Config: &tls.Config{ServerName: host, MinVersion: tls.VersionTLS10, MaxVersion: max}} //nolint:gosec // G402: offering an old version is the check
	ctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()
	conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort(host, "443"))
	if err != nil {
		return err
	}
	return conn.Close()
}

// Check reports an error unless host refuses TLS 1.1 and accepts TLS 1.2.
// A nil dial is Direct.
func Check(ctx context.Context, host string, dial Dial) error {
	if dial == nil {
		dial = Direct
	}
	if err := dial(ctx, host, tls.VersionTLS12); err != nil {
		return fmt.Errorf("tlsmin: %s refuses TLS 1.2: %w", host, err)
	}
	if err := dial(ctx, host, tls.VersionTLS11); err == nil {
		return fmt.Errorf("tlsmin: %s accepts TLS 1.1; set its minimum TLS version to 1.2", host)
	}
	return nil
}
