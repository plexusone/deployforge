package cli

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/plexusone/deployforge/dbimport"
)

// TestImportHookRegistersItsFlags checks that a host tool's hook adds its
// flags to db import, and that the command refuses before reaching any
// store when the dump is missing or holds no COPY data.
func TestImportHookRegistersItsFlags(t *testing.T) {
	var accounts []string
	hook := func(cmd *cobra.Command) ImportReplacer {
		cmd.Flags().StringArrayVar(&accounts, "account", nil, "ID=ADDRESS")
		return func(context.Context, string, dbimport.Remote, io.Writer) ([]dbimport.Replacement, error) {
			return nil, nil
		}
	}
	root := New(Options{Fleet: testFleet, ImportHook: hook})
	cmd, _, err := root.Find([]string{"db", "import"})
	if err != nil || cmd.Flags().Lookup("account") == nil || cmd.Flags().Lookup("replace") == nil || cmd.Flags().Lookup("confirm") == nil {
		t.Fatalf("db import: %v", err)
	}

	empty := filepath.Join(t.TempDir(), "empty.sql")
	if err := os.WriteFile(empty, []byte("SELECT 1;\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"db", "import", "example-app", "prod", "app"},
		{"db", "import", "example-app", "prod", "app", "--file", empty},
		{"db", "import", "example-app", "prod", "app", "--file", empty, "--replace", "=x"},
	} {
		root := New(Options{Fleet: testFleet, ImportHook: hook})
		var out bytes.Buffer
		root.SetOut(&out)
		root.SetErr(&out)
		root.SetArgs(args)
		if err := root.Execute(); err == nil {
			t.Errorf("%s: no error", strings.Join(args, " "))
		}
	}
}
