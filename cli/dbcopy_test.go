package cli

import (
	"strings"
	"testing"
)

func TestDBCopyRefusesWithoutConfirmOrDirs(t *testing.T) {
	for name, tc := range map[string]struct {
		args []string
		want string
	}{
		"run unconfirmed":     {[]string{"db", "copy", "run", "example-app", "prod"}, "--confirm"},
		"promote unconfirmed": {[]string{"db", "copy", "promote", "example-app", "prod"}, "--confirm"},
		"an unknown dir":      {[]string{"db", "copy", "verify", "example-app", "prod", "--from", "old"}, "live, next or previous"},
	} {
		if _, err := run(t, tc.args...); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want %q", name, err, tc.want)
		}
	}
}
