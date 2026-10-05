package workbenchprovider

import (
	"encoding/base64"
	"encoding/json"
	"strconv"

	"github.com/goobers/goobers/internal/workbench"
)

type pageCursor struct {
	Version int    `json:"v"`
	Target  string `json:"target"`
	Limit   int    `json:"limit"`
	Native  string `json:"native"`
}

func (r *BacklogReader) parseCursor(request workbench.BacklogPageRequest) (int, string, error) {
	limit := request.Limit
	if limit < 0 || limit > workbench.MaxBacklogPageItems || len(request.Cursor) > workbench.MaxBacklogCursorBytes {
		return 0, "", ErrInvalidCursor
	}
	if request.Cursor == "" {
		if limit == 0 {
			limit = DefaultPageItems
		}
		return limit, "", nil
	}
	var cursor pageCursor
	raw, err := base64.RawURLEncoding.DecodeString(request.Cursor)
	if err != nil || json.Unmarshal(raw, &cursor) != nil || cursor.Version != 1 || cursor.Target != r.targetDigest || cursor.Limit < 1 || cursor.Limit > workbench.MaxBacklogPageItems || !positiveID(cursor.Native) || (limit != 0 && limit != cursor.Limit) {
		return 0, "", ErrInvalidCursor
	}
	// A cursor carries no URL, credential, provider selector or arbitrary query.
	return cursor.Limit, cursor.Native, nil
}

func (r *BacklogReader) encodeCursor(limit int, native string) string {
	raw, _ := json.Marshal(pageCursor{Version: 1, Target: r.targetDigest, Limit: limit, Native: native})
	return base64.RawURLEncoding.EncodeToString(raw)
}

func advancingCursor(previous, next string) bool {
	if !positiveID(next) {
		return false
	}
	if previous == "" {
		return true
	}
	before, _ := strconv.ParseInt(previous, 10, 64)
	after, _ := strconv.ParseInt(next, 10, 64)
	return after > before
}
