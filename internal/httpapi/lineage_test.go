package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/readservice"
)

type pagedLineageReader struct {
	fakeReader
	after string
	runID string
}

func (r *pagedLineageReader) GetRunWithContinuations(_ context.Context, id, after string) (readservice.RunDetail, error) {
	r.runID, r.after = id, after
	return readservice.RunDetail{RunSummary: readservice.RunSummary{ID: id, Lineage: &readservice.RunLineage{ContinuationsNextCursor: "next", ContinuationsTruncated: true}}}, nil
}
func TestRunDetailPassesContinuationCursor(t *testing.T) {
	reader := &pagedLineageReader{}
	handler, err := NewHandler(reader, AllowAll, discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, RunsPath+"/source?continuationsAfter=child-049", nil))
	if response.Code != 200 || reader.runID != "source" || reader.after != "child-049" || !strings.Contains(response.Body.String(), `"continuationsNextCursor":"next"`) {
		t.Fatalf("code=%d cursor=%q body=%s", response.Code, reader.after, response.Body.String())
	}
}
