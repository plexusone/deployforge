package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestGeneratorToolNamesNoMachine checks that a generated page names the
// host tool when one is given, and otherwise this program by name, never
// a path such as go run's temporary build directory.
func TestGeneratorToolNamesNoMachine(t *testing.T) {
	if got := generatorTool([]string{"bin/example", "--fleet", "fleet.json"}); got != "bin/example" {
		t.Errorf("with a runbook tool: %q", got)
	}
	saved := os.Args[0]
	defer func() { os.Args[0] = saved }()
	os.Args[0] = filepath.Join(os.TempDir(), "go-build123", "b001", "exe", "deployforge")
	if got := generatorTool(nil); got != "deployforge" {
		t.Errorf("under go run: %q", got)
	}
	if got := program(); strings.Contains(got, string(filepath.Separator)) {
		t.Errorf("program() under go run is a path: %q", got)
	}
}
