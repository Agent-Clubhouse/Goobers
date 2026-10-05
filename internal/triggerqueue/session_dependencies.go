package triggerqueue

import (
	"context"
	"errors"
	"strings"
)

// RetainedSessionGenerations includes idle conversations that have not created
// a run yet. Their immutable profile cannot depend on a later run-directory scan.
// A finite distinct-pin ceiling refuses inventory instead of dropping an owner.
func (s *Store) RetainedSessionGenerations(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT json_extract(profile,'$.configGeneration') FROM interactive_sessions ORDER BY 1 LIMIT 4097`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var generations []string
	for rows.Next() {
		var generation string
		if err = rows.Scan(&generation); err != nil {
			return nil, err
		}
		if len(generation) != 71 || !strings.HasPrefix(generation, "sha256:") || strings.Trim(generation[7:], "0123456789abcdef") != "" {
			return nil, errors.New("invalid retained session generation")
		}
		generations = append(generations, generation)
		if len(generations) > 4096 {
			return nil, errors.New("retained session generation inventory exceeds bound")
		}
	}
	return generations, rows.Err()
}
