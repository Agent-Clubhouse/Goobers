package branchretention

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/instanceannotations"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/worktree"
	"github.com/goobers/goobers/providers"
)

func TestServicePreservesOwnershipAndRechecksSiblingBeforePruning(t *testing.T) {
	root := t.TempDir()
	for _, id := range []string{"owner", "sibling"} {
		run, err := journal.Create(root, journal.RunIdentity{RunID: id, Workflow: "fixture", WorkflowVersion: 1, Gaggle: "web", Trigger: journal.Trigger{Kind: journal.TriggerItem, Ref: "17"}}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := run.Append(journal.Event{Type: journal.EventRunFinished, Status: string(journal.PhaseCompleted)}); err != nil {
			t.Fatal(err)
		}
		if err := run.Close(); err != nil {
			t.Fatal(err)
		}
	}
	unknown := errors.New("unknown ownership")
	records := map[string]instanceannotations.ItemRepository{}
	parked := false
	calls := 0
	service := Service{Root: root, UnknownRepository: unknown, KindAuthorizesCustody: func(kind string) bool { return kind == "issue" }, Repositories: func(string) (map[string]instanceannotations.ItemRepository, error) { return records, nil }, ItemParked: func(_ context.Context, _ string, id string, record instanceannotations.ItemRepository) (bool, error) {
		calls++
		if id != "17" || record.Repository.Owner != "recorded-owner" {
			t.Fatal("guessed ownership")
		}
		return parked, nil
	}}
	if allowed, err := service.Allowed(t.Context(), root, "owner"); allowed || !errors.Is(err, unknown) || calls != 0 {
		t.Fatalf("missing authority: %v %v", allowed, err)
	}
	records["17"] = instanceannotations.ItemRepository{Repository: providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "recorded-owner", Name: "repo"}, Kind: "issue"}
	options := worktree.RetentionOptions{Now: time.Now().Add(time.Second)}
	service.Configure(t.Context(), map[string]string{"root": root}, map[string]map[string][]string{"root": {"branch": {"owner", "sibling"}}}, &options)
	if terminal, err := options.IsRunTerminal("root", "owner"); !terminal || err != nil {
		t.Fatalf("terminal: %v %v", terminal, err)
	}
	if ended, err := options.RunTerminalAt("root", "owner"); ended.IsZero() || err != nil {
		t.Fatalf("age: %v %v", ended, err)
	}
	if protected, err := options.IsBranchProtected("root", "branch"); protected || err != nil {
		t.Fatalf("protection: %v %v", protected, err)
	}
	if allowed, err := options.CanPruneBranch("root", "owner", "branch"); !allowed || err != nil || calls != 2 {
		t.Fatalf("discovery: %v %v %d", allowed, err, calls)
	}
	parked = true
	if allowed, err := options.CanPruneBranch("root", "owner", "branch"); allowed || err != nil || calls != 3 {
		t.Fatalf("fresh pre-delete check: %v %v %d", allowed, err, calls)
	}
	records["17"] = instanceannotations.ItemRepository{}
	if allowed, err := service.Allowed(t.Context(), root, "owner"); allowed || !errors.Is(err, unknown) {
		t.Fatalf("invalid ownership: %v %v", allowed, err)
	}
	if protected, err := options.IsBranchProtected("root", "missing"); protected || err != nil {
		t.Fatalf("unreferenced branch: %v %v", protected, err)
	}
}
