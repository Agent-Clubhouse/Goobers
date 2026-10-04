package workbenchprovider

import (
	"encoding/base64"
	"encoding/json"

	"github.com/goobers/goobers/internal/workbench"
	"github.com/goobers/goobers/providers"
)

type documentCursor struct {
	Version        int
	Target, Commit string
	Offset, Limit  int
}

func (r *RepositoryReader) documentCursor(request workbench.DocumentPageRequest) (documentCursor, error) {
	cursor := documentCursor{Version: 1, Target: r.targetDigest, Limit: request.Limit}
	if cursor.Limit < 0 || cursor.Limit > workbench.MaxDocumentPageFiles || len(request.Cursor) > workbench.MaxBacklogCursorBytes {
		return cursor, ErrInvalidCursor
	}
	if request.Cursor == "" {
		if cursor.Limit == 0 {
			cursor.Limit = workbench.MaxDocumentPageFiles
		}
		return cursor, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(request.Cursor)
	if err != nil || json.Unmarshal(raw, &cursor) != nil {
		return cursor, ErrInvalidCursor
	}
	if cursor.Version != 1 || cursor.Target != r.targetDigest || !providers.ValidSourceCommit(cursor.Commit) || cursor.Offset < 1 || cursor.Offset >= len(r.paths) || cursor.Limit < 1 || cursor.Limit > workbench.MaxDocumentPageFiles || (request.Limit != 0 && request.Limit != cursor.Limit) {
		return cursor, ErrInvalidCursor
	}
	return cursor, nil
}

func (r *RepositoryReader) encodeDocumentCursor(cursor documentCursor) string {
	raw, _ := json.Marshal(cursor)
	return base64.RawURLEncoding.EncodeToString(raw)
}
