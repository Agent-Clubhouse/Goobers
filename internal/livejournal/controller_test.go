package livejournal

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

type fixtureControllerMinter struct{}

func (fixtureControllerMinter) MintControllerJournal(string, string, time.Duration) (string, error) {
	return ControllerJournalTokenPrefix + "fixture", nil
}

func TestControllerEmitterRefusesRedirect(t *testing.T) {
	received := false
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { received = true }))
	defer target.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer source.Close()
	client := HTTPEmitter{BaseURL: source.URL, ControllerMinter: fixtureControllerMinter{}, Client: source.Client()}
	if _, err := client.Emit(t.Context(), EmitRequest{RunID: "run-1"}); err == nil {
		t.Fatal("redirect accepted")
	}
	if received {
		t.Fatal("controller capability followed redirect")
	}
}
