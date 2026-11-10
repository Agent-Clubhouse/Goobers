package httpapi

import (
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// decodingPlane decodes into a map the given number of times, then returns err.
type decodingPlane struct {
	decodes int
	err     error
}

func (p decodingPlane) Admit(_ context.Context, decode func(any) error) (any, error) {
	for range p.decodes {
		var in map[string]any
		if err := decode(&in); err != nil {
			return nil, err
		}
	}
	return map[string]string{"ok": "yes"}, p.err
}
func (p decodingPlane) Load(context.Context, func(any) error) ([]byte, error) { return nil, nil }
func (p decodingPlane) Cancel(context.Context, func(any) error) error         { return nil }

func TestEngineChildPlaneDecodesExactlyOnceAndMapsErrors(t *testing.T) {
	for name, tc := range map[string]struct {
		plane decodingPlane
		body  string
		want  int
	}{
		"decoded once":    {plane: decodingPlane{decodes: 1}, body: `{"a":1}`, want: http.StatusOK},
		"malformed body":  {plane: decodingPlane{decodes: 1}, body: `{"a":`, want: http.StatusBadRequest},
		"trailing object": {plane: decodingPlane{decodes: 1}, body: `{"a":1}{}`, want: http.StatusBadRequest},
		"never decoded":   {plane: decodingPlane{decodes: 0}, body: `{"a":1}`, want: http.StatusInternalServerError},
		"decoded twice":   {plane: decodingPlane{decodes: 2}, body: `{"a":1}`, want: http.StatusInternalServerError},
		"custody failure": {plane: decodingPlane{decodes: 1, err: errors.New("custody down")}, body: `{"a":1}`, want: http.StatusInternalServerError},
	} {
		t.Run(name, func(t *testing.T) {
			config := handlerConfig{engineChildren: tc.plane}
			handler := handleEngineChild(config.engineChildren != nil, log.New(io.Discard, "", 0), "admit engine child",
				func(ctx context.Context, decode func(any) error) (any, error) {
					return config.engineChildren.Admit(ctx, decode)
				},
				func(w http.ResponseWriter, out any) { writeJSON(w, http.StatusOK, out) })
			request := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(tc.body))
			request.Header.Set("Content-Type", "application/json")
			principal := Principal{Subject: "worker:engine", Issuer: WorkerChildAdmissionPrincipalIssuer}
			request = request.WithContext(context.WithValue(request.Context(), principalContextKey{}, principal))
			recorder := httptest.NewRecorder()
			handler(recorder, request)
			if recorder.Code != tc.want {
				t.Fatalf("status = %d, want %d (%s)", recorder.Code, tc.want, recorder.Body)
			}
		})
	}
}
