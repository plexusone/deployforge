// Package readiness checks an app's host names from outside, as a visitor
// reaches them: https://<host>/readyz must answer 200 through whatever is in
// front of the host, such as a CDN and its firewall.
package readiness

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Checker probes host names.
type Checker struct {
	// Client sends the probes; one with a 10-second timeout when nil.
	Client *http.Client
	// Attempts is how many times each host is probed before it fails; 5
	// when zero.
	Attempts int
	// Wait is the pause between attempts; 3 seconds when zero.
	Wait time.Duration
	// Scheme is https when empty; tests set http.
	Scheme string
	// Sleep waits between attempts; time.Sleep when nil.
	Sleep func(time.Duration)
}

// Check probes each host until it answers 200 or its attempts run out, and
// reports every host that did not.
func (c Checker) Check(ctx context.Context, hosts []string) error {
	client := c.Client
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	attempts := c.Attempts
	if attempts <= 0 {
		attempts = 5
	}
	wait := c.Wait
	if wait <= 0 {
		wait = 3 * time.Second
	}
	sleep := c.Sleep
	if sleep == nil {
		sleep = time.Sleep
	}
	scheme := c.Scheme
	if scheme == "" {
		scheme = "https"
	}
	var errs []error
	for _, h := range hosts {
		var last error
		for i := 0; i < attempts; i++ {
			if i > 0 {
				sleep(wait)
			}
			if last = probe(ctx, client, scheme+"://"+h+"/readyz"); last == nil {
				break
			}
			if ctx.Err() != nil {
				break
			}
		}
		if last != nil {
			errs = append(errs, fmt.Errorf("readiness: %s: %w", h, last))
		}
	}
	return errors.Join(errs...)
}

func probe(ctx context.Context, client *http.Client, url string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	res, err := client.Do(req)
	if err != nil {
		return err
	}
	// Drain a little so the connection can be reused; the body is not used.
	_, copyErr := io.Copy(io.Discard, io.LimitReader(res.Body, 4096))
	closeErr := res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("/readyz answered %d", res.StatusCode)
	}
	return errors.Join(copyErr, closeErr)
}
