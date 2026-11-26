// Package fleetpage writes a fleet's Markdown pages: its inventory, from the
// fleet document, and the fleet document's format reference, from the Go
// types the schema is generated from, so neither page can drift.
package fleetpage

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/invopop/jsonschema"

	"github.com/plexusone/deployforge/fleet"
)

// Inventory renders the fleet as a Markdown page: each app's stacks, its
// secrets and labels, each stack's unit and limits, and the apps' notes.
// source names the fleet document, and command the command that wrote the
// page, for its header.
func Inventory(f *fleet.Fleet, source, command string) string {
	var b strings.Builder
	b.WriteString("# Fleet Inventory\n\n")
	fmt.Fprintf(&b, "Generated from `%s` by `%s`; edit the fleet document, not this page.\n\n", source, command)
	b.WriteString("| App | Stack | Host | Host names | Port | Databases | Status |\n")
	b.WriteString("|---|---|---|---|---|---|---|\n")
	for _, a := range f.Apps {
		for _, stack := range f.StackNames(a) {
			s := a.Stacks[stack]
			dbs := make([]string, len(s.Databases))
			for i, d := range s.Databases {
				dbs[i] = d.Name
			}
			fmt.Fprintf(&b, "| `%s` | %s | `%s` | %s | %d | %s | %s |\n",
				a.Name, stack, s.Host, code(s.Hosts), s.Port, code(dbs), s.Status)
		}
	}
	b.WriteString("\n## Secrets and Labels\n\n")
	b.WriteString("| App | Secret prefix | Edge | Labels |\n")
	b.WriteString("|---|---|---|---|\n")
	for _, a := range f.Apps {
		var labels []string
		for _, k := range slices.Sorted(maps.Keys(a.Labels)) {
			labels = append(labels, fmt.Sprintf("`%s`: %s", k, cell(a.Labels[k])))
		}
		fmt.Fprintf(&b, "| `%s` | `%s` | %s | %s |\n", a.Name, a.SecretPrefix, orNone(string(a.Edge)), orNone(strings.Join(labels, "; ")))
	}
	b.WriteString("\n## Units and Resource Limits\n\n")
	b.WriteString("| App | Stack | Unit | Unix user | `MemoryMax` | `CPUQuota` | Connection limit |\n")
	b.WriteString("|---|---|---|---|---|---|---|\n")
	for _, a := range f.Apps {
		for _, stack := range f.StackNames(a) {
			mem, cpu, conn := "not set", "not set", "not set"
			if l := a.Stacks[stack].Limits; l != nil {
				mem, cpu, conn = l.MemoryMax, l.CPUQuota, fmt.Sprint(l.ConnectionLimit)
			}
			fmt.Fprintf(&b, "| `%s` | %s | `%s` | `%s` | %s | %s | %s |\n", a.Name, stack, f.Unit(a, stack), f.UnixUser(a, stack), mem, cpu, conn)
		}
	}
	var notes []string
	for _, a := range f.Apps {
		if a.Notes != "" {
			notes = append(notes, fmt.Sprintf("- **`%s`:** %s", a.Name, a.Notes))
		}
	}
	if len(notes) > 0 {
		b.WriteString("\n## Notes\n\n")
		b.WriteString(strings.Join(notes, "\n"))
		b.WriteString("\n")
	}
	return b.String()
}

// Format renders the fleet document's format reference from the schema the
// Go types generate: each object's fields in order, with their type,
// whether they are required, their limits and their meaning. command names
// the command that wrote it, for its header.
func Format(command string) (string, error) {
	r := &jsonschema.Reflector{}
	root := r.Reflect(&fleet.Fleet{})
	name, ok := strings.CutPrefix(root.Ref, "#/$defs/")
	if !ok {
		return "", fmt.Errorf("fleetpage: format: the schema's root is %q, not a definition", root.Ref)
	}
	var b strings.Builder
	b.WriteString("# Fleet Document Format\n\n")
	fmt.Fprintf(&b, "Generated from deployforge's Go types by `%s`; this page follows the\n", command)
	b.WriteString("types, as the JSON Schema does.\n\n")
	fmt.Fprintf(&b, "**Format version:** `%s`\n\n", fleet.FormatVersion)
	b.WriteString("A document names its version in `formatVersion`. Unknown fields are an error.\n")
	b.WriteString("Property names are camelCase. JSON has no comments: an app's `notes` field says\n")
	b.WriteString("why it is set up as it is.\n")

	seen := map[string]bool{}
	queue := []string{name}
	for len(queue) > 0 {
		n := queue[0]
		queue = queue[1:]
		if seen[n] {
			continue
		}
		seen[n] = true
		def, ok := root.Definitions[n]
		if !ok {
			return "", fmt.Errorf("fleetpage: format: no definition %q", n)
		}
		fmt.Fprintf(&b, "\n## %s\n\n", n)
		if def.Description != "" {
			fmt.Fprintf(&b, "%s\n\n", def.Description)
		}
		b.WriteString("| Field | Type | Required | Limits | Meaning |\n")
		b.WriteString("|---|---|---|---|---|\n")
		for p := def.Properties.Oldest(); p != nil; p = p.Next() {
			typ, refs := typeOf(p.Value)
			queue = append(queue, refs...)
			req := "no"
			if slices.Contains(def.Required, p.Key) {
				req = "yes"
			}
			fmt.Fprintf(&b, "| `%s` | %s | %s | %s | %s |\n", p.Key, typ, req, limits(p.Value), cell(p.Value.Description))
		}
	}
	return b.String(), nil
}

// typeOf describes a property's type, linking definitions, and returns the
// definitions it refers to.
func typeOf(s *jsonschema.Schema) (string, []string) {
	if n, ok := strings.CutPrefix(s.Ref, "#/$defs/"); ok {
		return fmt.Sprintf("[%s](#%s)", n, strings.ToLower(n)), []string{n}
	}
	if s.Type == "array" && s.Items != nil {
		inner, refs := typeOf(s.Items)
		return "array of " + inner, refs
	}
	if s.Type == "object" && s.AdditionalProperties != nil {
		inner, refs := typeOf(s.AdditionalProperties)
		return "map of " + inner, refs
	}
	if s.Type == "" {
		return "any", nil
	}
	return s.Type, nil
}

// limits lists a property's constraints, including those of array items.
func limits(s *jsonschema.Schema) string {
	var out []string
	add := func(t *jsonschema.Schema) {
		if len(t.Enum) > 0 {
			vals := make([]string, len(t.Enum))
			for i, v := range t.Enum {
				vals[i] = fmt.Sprintf("`%v`", v)
			}
			out = append(out, "one of "+strings.Join(vals, ", "))
		}
		if t.Pattern != "" {
			out = append(out, "matches `"+strings.ReplaceAll(t.Pattern, "|", `\|`)+"`")
		}
		if t.Minimum != "" {
			out = append(out, "at least "+t.Minimum.String())
		}
		if t.Maximum != "" {
			out = append(out, "at most "+t.Maximum.String())
		}
		if t.MinItems != nil {
			out = append(out, fmt.Sprintf("at least %d item(s)", *t.MinItems))
		}
		if t.MinProperties != nil {
			out = append(out, fmt.Sprintf("at least %d entry(ies)", *t.MinProperties))
		}
	}
	add(s)
	if s.Type == "array" && s.Items != nil && s.Items.Ref == "" {
		add(s.Items)
	}
	return strings.Join(out, "; ")
}

func code(items []string) string {
	if len(items) == 0 {
		return "none"
	}
	out := make([]string, len(items))
	for i, it := range items {
		out[i] = "`" + it + "`"
	}
	return strings.Join(out, ", ")
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}

// cell escapes text for a Markdown table cell: pipes, and angle brackets,
// which renderers would otherwise read as HTML tags.
func cell(s string) string {
	return strings.NewReplacer("|", `\|`, "<", "&lt;", ">", "&gt;").Replace(s)
}
