package secrets

import (
	"context"
	"errors"
	"strings"
	"testing"
)

const prefix = "/example/prod/one-cloud/"

const pem = "-----BEGIN PRIVATE KEY-----\nabc\n-----END PRIVATE KEY-----"

// fakeStore is a parameter store in memory, keyed by full name. ByPath returns
// only the parameters directly under a path, as SSM does without recursion.
type fakeStore struct {
	params map[string]string
	err    error
}

func (s fakeStore) ByPath(_ context.Context, path string) ([]Param, error) {
	if s.err != nil {
		return nil, s.err
	}
	var out []Param
	for name, value := range s.params {
		if rest, ok := strings.CutPrefix(name, path); ok && !strings.Contains(rest, "/") {
			out = append(out, Param{Name: name, Value: value})
		}
	}
	return out, nil
}

func TestLoadSplitsTheEnvironmentMigrateVariablesAndFiles(t *testing.T) {
	s := fakeStore{params: map[string]string{
		prefix + "SESSION_KEY":               "s3cr3t",
		prefix + "PG_DSN":                    "postgres://app@db/one",
		prefix + "migrate/ADMIN_DSN":         "postgres://owner@db/one",
		prefix + "files/signing-key.pem":     pem,
		prefix + "files/notes.txt":           "plain",
		"/example/staging/one-cloud/IGNORED": "staging",
	}}
	b, err := Load(context.Background(), s, prefix)
	if err != nil {
		t.Fatal(err)
	}
	if len(b.Env) != 2 || b.Env[0].Name != "PG_DSN" || b.Env[1].Name != "SESSION_KEY" {
		t.Errorf("env = %+v", b.Env)
	}
	if len(b.Migrate) != 1 || b.Migrate[0] != (Var{Name: "ADMIN_DSN", Value: "postgres://owner@db/one"}) {
		t.Errorf("migrate = %+v", b.Migrate)
	}
	if len(b.Files) != 2 || b.Files[0].Name != "notes.txt" || string(b.Files[1].Data) != pem+"\n" {
		t.Errorf("files = %+v", b.Files)
	}
	if got := string(EnvFile(b.Env)); got != "PG_DSN='postgres://app@db/one'\nSESSION_KEY='s3cr3t'\n" {
		t.Errorf("env file = %q", got)
	}
	if v := b.Values(); len(v) != 5 {
		t.Errorf("values = %d, want 5", len(v))
	}
}

func TestMigrateAndFilesAreOptionalButTheEnvironmentIsNot(t *testing.T) {
	if _, err := Load(context.Background(), fakeStore{params: map[string]string{prefix + "A": "1"}}, prefix); err != nil {
		t.Errorf("environment only: %v", err)
	}
	_, err := Load(context.Background(), fakeStore{params: map[string]string{prefix + "migrate/A": "1"}}, prefix)
	if err == nil || !strings.Contains(err.Error(), "no parameters under") {
		t.Errorf("no environment: %v", err)
	}
}

func TestLoadRefusesWhatTheFilesCannotHold(t *testing.T) {
	for name, params := range map[string]map[string]string{
		"an invalid variable name":   {prefix + "1ABC": "x"},
		"a dash in a variable name":  {prefix + "A-B": "x"},
		"a multi-line value":         {prefix + "A": "one\ntwo"},
		"a carriage return":          {prefix + "A": "one\rtwo"},
		"a single quote":             {prefix + "A": "it's"},
		"a migrate multi-line value": {prefix + "A": "1", prefix + "migrate/B": "x\ny"},
		"a hidden file name":         {prefix + "A": "1", prefix + "files/.env": "x"},
		"a .pem that is not a key":   {prefix + "A": "1", prefix + "files/key.pem": "not a key"},
	} {
		if _, err := Load(context.Background(), fakeStore{params: params}, prefix); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestLoadRefusesAParameterNotDirectlyUnderItsPath(t *testing.T) {
	// A store that returns deeper parameters despite asking for none.
	s := storeFunc(func(_ context.Context, path string) ([]Param, error) {
		return []Param{{Name: path + "deeper/A", Value: "1"}}, nil
	})
	if _, err := Load(context.Background(), s, prefix); err == nil {
		t.Error("accepted a parameter below its path")
	}
}

func TestLoadReportsTheStoresError(t *testing.T) {
	_, err := Load(context.Background(), fakeStore{err: errors.New("AccessDenied")}, prefix)
	if err == nil || !strings.Contains(err.Error(), "AccessDenied") {
		t.Errorf("err = %v", err)
	}
	if _, err := Load(context.Background(), fakeStore{}, "/no/trailing/slash"); err == nil {
		t.Error("accepted a prefix without a trailing slash")
	}
}

type storeFunc func(context.Context, string) ([]Param, error)

func (f storeFunc) ByPath(ctx context.Context, path string) ([]Param, error) { return f(ctx, path) }

func TestRedactor(t *testing.T) {
	b := Bundle{
		Env:     []Var{{Name: "TOKEN", Value: "s3cr3t-token"}, {Name: "SHORT", Value: "ab"}},
		Migrate: []Var{{Name: "PASSWORD", Value: "s3cr3t"}},
		Files:   []File{{Name: "key.pem", Data: []byte("-----BEGIN PRIVATE KEY-----\nAAAA\n-----END PRIVATE KEY-----\n")}},
	}
	r := NewRedactor(b, "extra-value")
	in := "token s3cr3t-token, password s3cr3t, short ab, extra-value, key -----BEGIN PRIVATE KEY-----\nAAAA\n-----END PRIVATE KEY-----"
	got := r.String(in)
	for _, leak := range []string{"s3cr3t", "extra-value", "AAAA"} {
		if strings.Contains(got, leak) {
			t.Errorf("%q leaks in %q", leak, got)
		}
	}
	// The longer value is masked whole, not as its shorter prefix.
	if !strings.Contains(got, "token "+Mask+",") {
		t.Errorf("token not masked whole: %q", got)
	}
	// Values too short to be secrets are left alone.
	if !strings.Contains(got, "short ab") {
		t.Errorf("short value masked: %q", got)
	}
}
