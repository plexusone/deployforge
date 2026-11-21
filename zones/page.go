package zones

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/invopop/jsonschema"
)

// SchemaID is the $id of zones.json's JSON Schema.
const SchemaID = "https://github.com/plexusone/deployforge/schema/zones.schema.json"

// JSONSchema generates zones.json's JSON Schema from the Go types, indented
// and ending in a newline, as it is committed.
func JSONSchema() ([]byte, error) {
	r := &jsonschema.Reflector{}
	s := r.Reflect(&Document{})
	s.ID = SchemaID
	s.Title = "DNS zones"
	s.Description = "Every DNS zone the fleet owns, each record and what it serves."
	out, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("zones: schema: %w", err)
	}
	return append(out, '\n'), nil
}

// Page renders the document as docs/reference/zones.md, which is
// generated, not edited.
func (d *Document) Page() string {
	var b strings.Builder
	b.WriteString("# DNS Zones\n\n")
	b.WriteString("Generated from `zones.json` by `deployforge zones page`; edit `zones.json`, not\n")
	b.WriteString("this page. It lists every record of every zone and what it serves, so a\n")
	b.WriteString("change to one zone is made knowing what else lives there.\n\n")
	b.WriteString("**Before changing a zone's records,** check the zone's name at the top of\n")
	b.WriteString("the Cloudflare dashboard, change `zones.json` with the records, and run\n")
	b.WriteString("`deployforge zones verify <zone>` before and after. It asks the zone's own\n")
	b.WriteString("name servers, so it sees a change at once, and fails naming any listed\n")
	b.WriteString("record that is missing or different. It cannot see a record the file does\n")
	b.WriteString("not list.\n\n")

	b.WriteString("## Websites\n\n")
	b.WriteString("| Host | Purpose | Served by | Source | Status |\n")
	b.WriteString("|---|---|---|---|---|\n")
	for _, z := range d.Zones {
		for _, r := range z.Records {
			if r.Purpose != StaticSite && r.Purpose != App && r.Purpose != Redirect || r.Type == "AAAA" {
				continue
			}
			src := r.Source
			if r.Purpose == Redirect && r.Check != nil {
				src = "redirect to " + r.Check.Location
			}
			fmt.Fprintf(&b, "| `%s` | %s | %s | %s | %s |\n", r.FQDN(z.Name), r.Purpose, orDash(r.Host), orDash(src), r.State())
		}
	}

	for _, z := range d.Zones {
		fmt.Fprintf(&b, "\n## %s\n\n", z.Name)
		fmt.Fprintf(&b, "Registered at %s; DNS answered by %s.\n", z.Registrar, z.DNS)
		if z.Notes != "" {
			fmt.Fprintf(&b, "\n%s\n", z.Notes)
		}
		if h := z.Hardening; h != nil {
			b.WriteString("\n**Hardening** (`deployforge zones harden`): the baseline")
			if len(h.AllowCountries) > 0 {
				fmt.Fprintf(&b, "; only %s on %s", strings.Join(h.AllowCountries, ", "), strings.Join(h.CountryHosts, " and "))
			}
			if h.BotFightModeOff {
				b.WriteString("; Bot Fight Mode off")
			}
			if h.WWWRedirect {
				b.WriteString("; www redirected to the apex")
			}
			b.WriteString(".\n")
		}
		b.WriteString("\n| Name | Type | Values | Proxied | Purpose | Served by | Status | Check |\n")
		b.WriteString("|---|---|---|---|---|---|---|---|\n")
		var notes []string
		for _, r := range z.Records {
			proxied := "no"
			if r.Proxied {
				proxied = "yes"
			}
			check := "-"
			if c := r.Check; c != nil {
				check = fmt.Sprintf("`%s` → %d", c.Path, c.Status)
				if c.Location != "" {
					check += " `" + c.Location + "`"
				}
			}
			vals := make([]string, len(r.Values))
			for i, v := range r.Values {
				if len(v) > 60 {
					v = v[:57] + "..."
				}
				vals[i] = "`" + strings.ReplaceAll(v, "|", "\\|") + "`"
			}
			fmt.Fprintf(&b, "| `%s` | %s | %s | %s | %s | %s | %s | %s |\n",
				r.Name, r.Type, strings.Join(vals, "<br>"), proxied, r.Purpose, orDash(r.Host), r.State(), check)
			if r.Notes != "" {
				notes = append(notes, fmt.Sprintf("- **`%s` %s:** %s", r.Name, r.Type, r.Notes))
			}
		}
		if len(notes) > 0 {
			b.WriteString("\n")
			b.WriteString(strings.Join(notes, "\n"))
			b.WriteString("\n")
		}
	}
	return b.String()
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
