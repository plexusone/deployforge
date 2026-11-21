// Command gen writes the fleet document's JSON Schema into the schema
// package. Run it with go generate from the schema directory.
package main

import (
	"fmt"
	"os"

	"github.com/plexusone/deployforge/schema"
	"github.com/plexusone/deployforge/zones"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "gen:", err)
		os.Exit(1)
	}
}

func run() error {
	data, err := schema.Generate()
	if err != nil {
		return err
	}
	// The schemas are committed and read by editors.
	if err := os.WriteFile(schema.FleetFile, data, 0o644); err != nil { //nolint:gosec // G306: a public schema file
		return err
	}
	z, err := zones.JSONSchema()
	if err != nil {
		return err
	}
	return os.WriteFile(schema.ZonesFile, z, 0o644) //nolint:gosec // G306: a public schema file
}
