package readiness

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestCheckRetriesUntilEachHostIsReady(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/readyz" {
			http.NotFound(w, r)
			return
		}
		if calls.Add(1) < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	host := strings.TrimPrefix(srv.URL, "http://")
	slept := 0
	c := Checker{Scheme: "http", Attempts: 5, Sleep: func(time.Duration) { slept++ }}
	if err := c.Check(context.Background(), []string{host}); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 3 || slept != 2 {
		t.Errorf("calls %d, sleeps %d", calls.Load(), slept)
	}
}

func TestCheckReportsEveryHostThatIsNotReady(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()
	host := strings.TrimPrefix(srv.URL, "http://")
	c := Checker{Scheme: "http", Attempts: 2, Sleep: func(time.Duration) {}}
	err := c.Check(context.Background(), []string{host, "127.0.0.1:1"})
	if err == nil || !strings.Contains(err.Error(), "answered 502") || !strings.Contains(err.Error(), "127.0.0.1:1") {
		t.Errorf("err = %v", err)
	}
}
