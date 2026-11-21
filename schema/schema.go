// Package schema holds the fleet document's JSON Schema, generated from the
// Go types in package fleet.
//
// Regenerate it after changing the types:
//
//	go generate ./schema
package schema

import (
	_ "embed"
	"encoding/json"
	"fmt"

	"github.com/invopop/jsonschema"

	"github.com/plexusone/deployforge/fleet"
)

//go:generate go run ./gen

// FleetID is the fleet document schema's ID.
const FleetID = "https://github.com/plexusone/deployforge/schema/fleet.schema.json"

// FleetFile is the schema's file name, relative to this package's directory.
const FleetFile = "fleet.schema.json"

//go:embed fleet.schema.json
var fleetSchema []byte

// Fleet returns the fleet document's JSON Schema.
func Fleet() []byte { return fleetSchema }

// Generate builds the fleet document's schema from the Go types.
func Generate() ([]byte, error) {
	r := &jsonschema.Reflector{}
	s := r.Reflect(&fleet.Fleet{})
	s.ID = FleetID
	s.Title = "Fleet"
	s.Description = "The hosts, the apps deployforge deploys to them, and each app's stacks."
	out, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("schema: %w", err)
	}
	return append(out, '\n'), nil
}
