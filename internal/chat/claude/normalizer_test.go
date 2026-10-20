package claude

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/chat/chattest"
)

func TestFixtureNormalization(t *testing.T) {
	paths, err := filepath.Glob("testdata/*.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range paths {
		t.Run(filepath.Base(path), func(t *testing.T) {
			f, err := os.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = f.Close() }()
			fixture, err := chattest.ReadFixture(f)
			if err != nil {
				t.Fatal(err)
			}
			n := &normalizer{}
			counts := map[string]int{}
			var costs []float64
			for _, entry := range fixture.Entries {
				if entry.From != "cli" || strings.HasPrefix(str(entry.Msg["type"]), "control_") {
					continue
				}
				for _, e := range n.mapFrame(entry.Msg) {
					counts[e.kind]++
					if cost, ok := e.data["costUsd"].(float64); ok {
						costs = append(costs, cost)
					}
					data, _ := json.Marshal(e.data)
					for _, forbidden := range []string{"rawInput", "updatedInput", "account", "permission_suggestions"} {
						if strings.Contains(string(data), forbidden) {
							t.Fatalf("raw data exposed: %s", forbidden)
						}
					}
				}
			}
			if counts["turn_ended"] == 0 {
				t.Fatal("no turn completion")
			}
			if filepath.Base(path) == "multi-turn.jsonl" {
				if len(costs) != 2 || math.Abs(costs[1]-0.0013) > 1e-10 {
					t.Fatalf("costs: %v", costs)
				}
			}
			if filepath.Base(path) == "turn-error.jsonl" && counts["error"] != 1 {
				t.Fatalf("errors: %d", counts["error"])
			}
			if filepath.Base(path) == "schema-drift.jsonl" && counts["protocol_unknown"] == 0 {
				t.Fatal("drift hidden")
			}
		})
	}
}
func TestResumedCostAndReset(t *testing.T) {
	n := &normalizer{resumed: true}
	result := func(cost float64) object {
		return object{"type": "result", "subtype": "success", "total_cost_usd": cost}
	}
	if _, ok := n.mapFrame(result(10))[0].data["costUsd"]; ok {
		t.Fatal("charged old resumed usage")
	}
	if got := n.mapFrame(result(12))[0].data["costUsd"]; got != float64(2) {
		t.Fatal(got)
	}
	if got := n.mapFrame(result(1))[0].data["costUsd"]; got != float64(1) {
		t.Fatal(got)
	}
}
