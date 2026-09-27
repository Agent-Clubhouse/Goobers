package dispatcher

import (
	"slices"
	"testing"
)

func TestAgenticMergeReceivesOnlyScopedJournalAuthority(t *testing.T) {
	minter := &recordingMinter{}
	d := &Dispatcher{cfg: Config{WriteAPIBase: "http://daemon", TokenMinter: minter}}
	attempt := Attempt{RunID: "run-1", Gaggle: "example", Agentic: true, Stage: "merge", PodToken: "parent-only", Capabilities: []string{"github:pr:merge"}}
	if err := d.mintPlaneTokens(&attempt); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(minter.scopes, []string{scopeJournal}) {
		t.Fatalf("broader authority minted: %v", minter.scopes)
	}
	env := agenticMergeJournalEnv(d.cfg, attempt)
	if len(env) != 2 || env[0].Name != JournalEndpointEnv || env[1].Name != JournalTokenEnv || env[1].Value == attempt.PodToken {
		t.Fatalf("wrong scoped environment: %+v", env)
	}
	if env := planeEnv(d.cfg, attempt); len(env) != 0 {
		t.Fatalf("agentic stage received all machine planes: %+v", env)
	}
	attempt.Capabilities = nil
	if env := agenticMergeJournalEnv(d.cfg, attempt); len(env) != 0 {
		t.Fatal("non-merge agent received authority")
	}
}
