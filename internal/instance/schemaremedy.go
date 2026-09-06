package instance

import (
	"fmt"
	"regexp"
	"strings"
)

// SchemaVersionRemedy is the outcome of RemedyInstanceSchemaVersion.
type SchemaVersionRemedy struct {
	// Changed is false when the file already pairs runners: with the right
	// schemaVersion, or declares no runners: at all — in both cases After is
	// the input unchanged.
	Changed bool
	// After is the remedied file content.
	After string
	// Note explains what was done (or why nothing was), for the CLI to print.
	Note string
}

// topLevelKey matches a top-level mapping key: column zero, no leading dash,
// so a `runners:` nested under another key or appearing inside a block scalar
// is not mistaken for the inventory. instance.yaml is a flat top-level mapping
// (api/schemas/instance.schema.json), which is what makes this safe.
var topLevelKey = regexp.MustCompile(`^([A-Za-z][A-Za-z0-9_]*):`)

// RemedyInstanceSchemaVersion is the one-line fix for the #4217 refusal: an
// instance.yaml that declares a runners: inventory must also declare
// schemaVersion: 2.
//
// It is a TEXT transform, deliberately, not a parse-and-remarshal. A
// remarshal would reformat and drop every comment in a file an operator owns
// and hand-edits — and the file it must repair is by definition one the
// strict loader currently REFUSES, so there is often nothing to parse into a
// Config in the first place. Inserting one line preserves the rest byte for
// byte, which is what makes the output reviewable as a diff.
func RemedyInstanceSchemaVersion(content string) (SchemaVersionRemedy, error) {
	lines := strings.Split(content, "\n")

	declaresRunners := false
	schemaLine := -1
	insertAfter := -1
	inBlockScalar := false
	for i, line := range lines {
		// A block scalar's body is indented under its key, so any column-zero
		// line ends it. Tracking this keeps a `runners:` written inside, say,
		// a multi-line description from reading as the inventory.
		if inBlockScalar {
			if strings.TrimSpace(line) == "" || line[0] == ' ' || line[0] == '\t' {
				continue
			}
			inBlockScalar = false
		}
		m := topLevelKey.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		if rest := strings.TrimSpace(line[len(m[0]):]); rest == "|" || rest == ">" ||
			strings.HasPrefix(rest, "|") || strings.HasPrefix(rest, ">") {
			inBlockScalar = true
		}
		switch m[1] {
		case "runners":
			declaresRunners = true
		case "schemaVersion":
			schemaLine = i
		case "apiVersion", "kind":
			insertAfter = i
		}
	}

	if !declaresRunners {
		return SchemaVersionRemedy{After: content, Note: "declares no runners: inventory; schemaVersion is not required"}, nil
	}
	want := fmt.Sprintf("schemaVersion: %d", InstanceSchemaVersionRunners)

	if schemaLine >= 0 {
		if strings.TrimSpace(lines[schemaLine]) == want {
			return SchemaVersionRemedy{After: content, Note: "already declares " + want}, nil
		}
		// A schemaVersion that is present but wrong is an operator's explicit
		// statement, not an omission. Rewriting it silently would discard a
		// deliberate value, so this reports rather than repairs.
		return SchemaVersionRemedy{After: content}, fmt.Errorf(
			"line %d declares %q, but a runners: inventory requires %q — this is an explicit value, not a missing line, so it is not rewritten automatically",
			schemaLine+1, strings.TrimSpace(lines[schemaLine]), want)
	}
	if insertAfter < 0 {
		return SchemaVersionRemedy{After: content}, fmt.Errorf(
			"found no top-level apiVersion: or kind: line to insert %q after; add it by hand", want)
	}

	out := make([]string, 0, len(lines)+1)
	out = append(out, lines[:insertAfter+1]...)
	out = append(out, want)
	out = append(out, lines[insertAfter+1:]...)
	return SchemaVersionRemedy{
		Changed: true,
		After:   strings.Join(out, "\n"),
		Note:    fmt.Sprintf("added %q (a runners: inventory is schema revision %d)", want, InstanceSchemaVersionRunners),
	}, nil
}
