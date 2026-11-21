// Package secrets reads a stack's secrets from its prefix in a parameter
// store into memory, in the three parts a deploy installs:
//
//	<prefix><VAR>          the unit's environment file
//	<prefix>migrate/<VAR>  the migrate step only, never kept on the host
//	<prefix>files/<name>   /etc/<unit>/<name>, one parameter per file
//
// Values must be on one line, except files, which may hold PEM keys.
package secrets

import (
	"bytes"
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Param is one parameter, decrypted.
type Param struct {
	Name  string
	Value string
}

// Store lists the parameters directly under a path, decrypted. Parameters
// under deeper paths are not returned.
type Store interface {
	ByPath(ctx context.Context, path string) ([]Param, error)
}

// Var is one environment variable.
type Var struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// File is one key file for the unit's configuration directory.
type File struct {
	Name string `json:"name"`
	Data []byte `json:"data"`
}

// Bundle is everything a deploy installs from the secret store.
type Bundle struct {
	Env     []Var  `json:"env"`
	Migrate []Var  `json:"migrate,omitempty"`
	Files   []File `json:"files,omitempty"`
}

var (
	envName  = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	fileName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
	pemKey   = regexp.MustCompile(`(?m)^-----BEGIN [A-Z0-9 ]*KEY-----\r?$`)
)

// Load reads a stack's secrets. The environment must hold at least one
// variable; migrate variables and files are optional.
func Load(ctx context.Context, s Store, prefix string) (Bundle, error) {
	if !strings.HasPrefix(prefix, "/") || !strings.HasSuffix(prefix, "/") {
		return Bundle{}, fmt.Errorf("secrets: prefix %q must begin and end with /", prefix)
	}
	var b Bundle
	var err error
	if b.Env, err = loadVars(ctx, s, prefix); err != nil {
		return Bundle{}, err
	}
	if len(b.Env) == 0 {
		return Bundle{}, fmt.Errorf("secrets: no parameters under %s", prefix)
	}
	if b.Migrate, err = loadVars(ctx, s, prefix+"migrate/"); err != nil {
		return Bundle{}, err
	}
	if b.Files, err = loadFiles(ctx, s, prefix+"files/"); err != nil {
		return Bundle{}, err
	}
	return b, nil
}

// leaf is a parameter's last path segment, after checking it is directly
// under path.
func leaf(path string, p Param) (string, error) {
	rest, ok := strings.CutPrefix(p.Name, path)
	if !ok || rest == "" || strings.Contains(rest, "/") {
		return "", fmt.Errorf("secrets: parameter %s is not directly under %s", p.Name, path)
	}
	return rest, nil
}

func loadVars(ctx context.Context, s Store, path string) ([]Var, error) {
	params, err := s.ByPath(ctx, path)
	if err != nil {
		return nil, fmt.Errorf("secrets: %s: %w", path, err)
	}
	vars := make([]Var, 0, len(params))
	for _, p := range params {
		name, err := leaf(path, p)
		if err != nil {
			return nil, err
		}
		if !envName.MatchString(name) {
			return nil, fmt.Errorf("secrets: refusing %s: not a valid environment variable name", p.Name)
		}
		if strings.ContainsAny(p.Value, "\r\n") {
			return nil, fmt.Errorf("secrets: refusing %s: environment values must be on one line; use files/ for multi-line values", p.Name)
		}
		if strings.Contains(p.Value, "'") {
			return nil, fmt.Errorf("secrets: refusing %s: single quotes are not supported in environment values", p.Name)
		}
		vars = append(vars, Var{Name: name, Value: p.Value})
	}
	sort.Slice(vars, func(i, j int) bool { return vars[i].Name < vars[j].Name })
	return vars, nil
}

func loadFiles(ctx context.Context, s Store, path string) ([]File, error) {
	params, err := s.ByPath(ctx, path)
	if err != nil {
		return nil, fmt.Errorf("secrets: %s: %w", path, err)
	}
	files := make([]File, 0, len(params))
	for _, p := range params {
		name, err := leaf(path, p)
		if err != nil {
			return nil, err
		}
		if !fileName.MatchString(name) {
			return nil, fmt.Errorf("secrets: refusing %s: not a plain file name", p.Name)
		}
		if strings.HasSuffix(name, ".pem") && !pemKey.MatchString(p.Value) {
			return nil, fmt.Errorf("secrets: %s is not a PEM key", p.Name)
		}
		data := []byte(p.Value)
		if len(data) > 0 && data[len(data)-1] != '\n' {
			data = append(data, '\n')
		}
		files = append(files, File{Name: name, Data: data})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Name < files[j].Name })
	return files, nil
}

// EnvFile renders variables in systemd's EnvironmentFile format, each value
// single-quoted. Load has already refused values a quote cannot hold.
func EnvFile(vars []Var) []byte {
	var b bytes.Buffer
	for _, v := range vars {
		fmt.Fprintf(&b, "%s='%s'\n", v.Name, v.Value)
	}
	return b.Bytes()
}

// Values lists every secret value in a bundle, for checking that none leaks
// into a log or a command line.
func (b Bundle) Values() []string {
	var out []string
	for _, v := range append(append([]Var{}, b.Env...), b.Migrate...) {
		out = append(out, v.Value)
	}
	for _, f := range b.Files {
		out = append(out, string(f.Data))
	}
	return out
}

// Redactor masks secret values in text, such as a command's output, before it
// is printed or logged.
type Redactor struct {
	r *strings.Replacer
}

// Mask replaces each secret value in text.
const Mask = "[redacted]"

// NewRedactor masks every value in the bundle and in extra. Values shorter
// than four bytes are not masked: they would mask ordinary text, and no
// secret is that short.
func NewRedactor(b Bundle, extra ...string) *Redactor {
	values := append(b.Values(), extra...)
	var pairs []string
	seen := map[string]bool{}
	for _, v := range values {
		for _, part := range []string{v, strings.TrimRight(v, "\n")} {
			if len(part) < 4 || seen[part] {
				continue
			}
			seen[part] = true
			pairs = append(pairs, part, Mask)
		}
	}
	// Longer values first, so a value that contains another is masked whole.
	sortPairsByLength(pairs)
	return &Redactor{r: strings.NewReplacer(pairs...)}
}

// String masks each secret value in s.
func (r *Redactor) String(s string) string { return r.r.Replace(s) }

func sortPairsByLength(pairs []string) {
	n := len(pairs) / 2
	idx := make([]int, n)
	for i := range idx {
		idx[i] = i
	}
	sort.SliceStable(idx, func(a, b int) bool { return len(pairs[2*idx[a]]) > len(pairs[2*idx[b]]) })
	sorted := make([]string, 0, len(pairs))
	for _, i := range idx {
		sorted = append(sorted, pairs[2*i], pairs[2*i+1])
	}
	copy(pairs, sorted)
}
