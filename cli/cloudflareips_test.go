package cli

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/plexusone/deployforge/deploy/cfranges"
)

// toServer sends every request to srv, whatever its host, so the fixed
// Cloudflare URLs reach the fake.
type toServer struct{ srv *url.URL }

func (s toServer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.URL.Scheme, r.URL.Host = s.srv.Scheme, s.srv.Host
	return http.DefaultTransport.RoundTrip(r)
}

// fakeCloudflare serves v4 and v6 as Cloudflare's published lists.
func fakeCloudflare(t *testing.T, v4, v6 string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := v4
		if r.URL.Path == "/ips-v6" {
			body = v6
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Error(err)
		}
	}))
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	cloudflareHTTP = &http.Client{Transport: toServer{u}}
	t.Cleanup(func() { cloudflareHTTP = nil })
}

func committed(t *testing.T) (v4, v6 string) {
	t.Helper()
	ranges, err := cfranges.Ranges()
	if err != nil {
		t.Fatal(err)
	}
	var a, b []string
	for _, r := range ranges {
		if r.Addr().Is4() {
			a = append(a, r.String())
		} else {
			b = append(b, r.String())
		}
	}
	return strings.Join(a, "\n") + "\n", strings.Join(b, "\n") + "\n"
}

func TestCloudflareIPsCheckAndWrite(t *testing.T) {
	v4, v6 := committed(t)
	fakeCloudflare(t, v4, v6)
	if out, err := run(t, "cloudflare-ips", "--check"); err != nil || !strings.Contains(out, "match") {
		t.Errorf("check with the same ranges: %q, %v", out, err)
	}

	fakeCloudflare(t, v4+"198.51.100.0/24\n", v6)
	if _, err := run(t, "cloudflare-ips", "--check"); err == nil || !strings.Contains(err.Error(), "added [198.51.100.0/24]") {
		t.Errorf("check with a new range: %v", err)
	}

	dir := t.TempDir()
	cmd := New(Options{Fleet: testFleet})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"cloudflare-ips", "--dir", dir})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("write: %v\n%s", err, out.String())
	}
	b, err := os.ReadFile(filepath.Join(dir, "ips-v4.txt")) //nolint:gosec // G304: the test's own file
	if err != nil || !strings.Contains(string(b), "198.51.100.0/24") || !strings.Contains(string(b), cfranges.URLv4) {
		t.Errorf("ips-v4.txt: %v\n%s", err, b)
	}
	if _, err := os.Stat(filepath.Join(dir, "ips-v6.txt")); err != nil {
		t.Errorf("ips-v6.txt: %v", err)
	}
}
