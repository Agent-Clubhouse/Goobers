// Package runlineage defines the shared bounds for informational lineage reads.
package runlineage

// PageSize bounds direct continuations; MaxDepth bounds ancestor point lookups.
const (
	PageSize = 50
	MaxDepth = 64
)

// Run is a direct continuation's indexed identity and display state.
type Run struct {
	ID    string
	Phase string
}

// Page uses an exclusive run-ID cursor. Incomplete means legacy rows have not
// yet been projected with authoritative continuation identity.
type Page struct {
	Runs       []Run
	NextCursor string
	Incomplete bool
}

// Trim removes the lookahead row while preserving the last returned cursor.
func (p *Page) Trim() {
	if len(p.Runs) > PageSize {
		p.Runs = p.Runs[:PageSize]
		p.NextCursor = p.Runs[PageSize-1].ID
	}
}
