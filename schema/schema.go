// Package schema holds the JSON Schemas of the fleet document and the zones
// document, generated from the Go types in packages fleet and zones.
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

// File names, relative to this package's directory.
const (
	FleetFile = "fleet.schema.json"
	ZonesFile = "zones.schema.json"
)

//go:embed fleet.schema.json
var fleetSchema []byte

//go:embed zones.schema.json
var zonesSchema []byte

// Fleet returns the fleet document's JSON Schema.
func Fleet() []byte { return fleetSchema }

// Zones returns the zones document's JSON Schema.
func Zones() []byte { return zonesSchema }

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
