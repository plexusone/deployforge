package onhost

import (
	"path/filepath"
	"testing"
)

// On the host Root is empty, and every path must still be absolute; the
// other tests all set a Root, so they never see this case.
func TestPathIsAbsoluteWithNoRoot(t *testing.T) {
	if got := (&Host{}).path("opt", "app", "releases"); got != filepath.FromSlash("/opt/app/releases") {
		t.Errorf("no root: %s", got)
	}
	root := t.TempDir()
	if got := (&Host{Root: root}).path("etc", "app"); got != filepath.Join(root, "etc", "app") {
		t.Errorf("a root: %s", got)
	}
}
