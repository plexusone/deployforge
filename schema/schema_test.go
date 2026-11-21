package schema

import (
	"bytes"
	"encoding/json"
	"testing"
)

// TestEmbeddedSchemaIsCurrent fails when the types changed and the schema
// was not regenerated with go generate ./schema.
func TestEmbeddedSchemaIsCurrent(t *testing.T) {
	got, err := Generate()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, Fleet()) {
		t.Error("fleet.schema.json is stale: run go generate ./schema")
	}
}

func TestSchemaContent(t *testing.T) {
	var s struct {
		ID   string `json:"$id"`
		Defs map[string]struct {
			Required   []string `json:"required"`
			Properties map[string]struct {
				Enum []string `json:"enum"`
			} `json:"properties"`
		} `json:"$defs"`
	}
	if err := json.Unmarshal(Fleet(), &s); err != nil {
		t.Fatal(err)
	}
	if s.ID != FleetID {
		t.Errorf("$id = %q", s.ID)
	}
	if e := s.Defs["Stack"].Properties["status"].Enum; len(e) != 3 {
		t.Errorf("Stack.status enum = %v", e)
	}
	if r := s.Defs["App"].Required; len(r) == 0 {
		t.Error("App has no required fields")
	}
}
