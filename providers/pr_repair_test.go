package providers

import (
	"errors"
	"strings"
	"testing"
)

func repairFixtureIntent(t *testing.T, f *repairFixture, reader PRRepairReader) PullRequestRepair {
	t.Helper()
	target, err := reader.InspectRepairPullRequest(t.Context(), f.repo(), "42")
	if err != nil {
		t.Fatal(err)
	}
	edited, added := "after", "new"
	value := PullRequestRepair{Target: target, CommandID: "repair-" + strings.Repeat("1", 32), Changes: []PRRepairChange{{Path: "edit.txt", PreviousBlob: sourceBlobID([]byte("before")), Content: &edited}, {Path: "added.txt", Content: &added}, {Path: "delete.txt", PreviousBlob: sourceBlobID([]byte("remove"))}}}
	value.Message = "Repair selected PR\n\nGoobers-Repair: " + value.CommandID + "\n"
	f.intent = value
	return value
}
func TestPRRepairNativeExpectedHeadAndLostResponse(t *testing.T) {
	for _, kind := range []ProviderKind{ProviderGitHub, ProviderADO} {
		for _, mode := range []string{"ack", "lost", "race"} {
			t.Run(string(kind)+"/"+mode, func(t *testing.T) {
				f, provider := newRepairFixture(t, kind)
				value := repairFixtureIntent(t, f, provider)
				f.mode = mode
				result, err := provider.ApplyPullRequestRepair(t.Context(), value)
				if f.attempts != 1 || !result.MutationAttempted || result.Acknowledged != (mode == "ack") || (err != nil) != (mode != "ack") {
					t.Fatal(result, err, f.attempts)
				}
				if mode == "race" {
					if f.head == repairNewSHA {
						t.Fatal("foreign head overwritten")
					}
					return
				}
				if mode == "ack" && result.CommitID != repairNewSHA {
					t.Fatal(result)
				}
				observation, err := provider.ObservePullRequestRepair(t.Context(), value)
				if err != nil || !observation.Matches || observation.CommitID != repairNewSHA || f.attempts != 1 {
					t.Fatal("read-only reconciliation", observation, err, f.attempts)
				}
			})
		}
	}
}
func TestPRRepairRefusesForeignOrChangedCustodyBeforeEffect(t *testing.T) {
	for _, kind := range []ProviderKind{ProviderGitHub, ProviderADO} {
		for _, mode := range []string{"fork", "foreign-repo", "denied-tree", "symlink", "executable", "truncated", "old-blob", "new-file-exists", "changed-head", "wrong-repository-id", "wrong-branch", "closed"} {
			t.Run(string(kind)+"/"+mode, func(t *testing.T) {
				f, provider := newRepairFixture(t, kind)
				value := repairFixtureIntent(t, f, provider)
				switch mode {
				case "old-blob":
					value.Changes[0].PreviousBlob = strings.Repeat("e", 40)
				case "new-file-exists":
					f.before["added.txt"] = "foreign"
				case "changed-head":
					f.head = strings.Repeat("e", 40)
				case "wrong-repository-id":
					value.Target.RepositoryID = "other"
				case "wrong-branch":
					value.Target.Head = "other"
				case "closed":
					value.Target.Open = false
				default:
					f.mode = mode
				}
				result, err := provider.ApplyPullRequestRepair(t.Context(), value)
				if err == nil || result.MutationAttempted || f.attempts != 0 {
					t.Fatal("unverified effect", result, err, f.attempts)
				}
			})
		}
	}
}
func TestPRRepairReadIsBoundedAndPinned(t *testing.T) {
	for _, kind := range []ProviderKind{ProviderGitHub, ProviderADO} {
		t.Run(string(kind), func(t *testing.T) {
			f, provider := newRepairFixture(t, kind)
			value := repairFixtureIntent(t, f, provider)
			file, err := provider.ReadRepairFile(t.Context(), value.Target, "edit.txt")
			if err != nil || !file.Present || file.Content != "before" || file.BlobID != value.Changes[0].PreviousBlob || file.Commit != repairOldSHA {
				t.Fatal(file, err)
			}
			missing, err := provider.ReadRepairFile(t.Context(), value.Target, "missing.txt")
			if err != nil || missing.Present || missing.Commit != repairOldSHA {
				t.Fatal(missing, err)
			}
			for _, name := range []string{"../token", ".git/config", "nested/.Goobers/token", ".goober-assets/key"} {
				if _, err = provider.ReadRepairFile(t.Context(), value.Target, name); !errors.Is(err, ErrPRRepair) {
					t.Fatal(name, err)
				}
			}
			f.head = repairNewSHA
			if _, err = provider.ReadRepairFile(t.Context(), value.Target, "edit.txt"); !errors.Is(err, ErrPRRepair) {
				t.Fatal("stale head read", err)
			}
		})
	}
}
func TestPRRepairReconciliationRequiresExactCommandAndDelta(t *testing.T) {
	for _, kind := range []ProviderKind{ProviderGitHub, ProviderADO} {
		for _, mode := range []string{"wrong-message", "extra-change", "wrong-content", "wrong-old-content", "truncated"} {
			t.Run(string(kind)+"/"+mode, func(t *testing.T) {
				f, provider := newRepairFixture(t, kind)
				value := repairFixtureIntent(t, f, provider)
				if _, err := provider.ApplyPullRequestRepair(t.Context(), value); err != nil {
					t.Fatal(err)
				}
				switch mode {
				case "wrong-content":
					f.after["edit.txt"] = "foreign"
				case "wrong-old-content":
					f.before["edit.txt"] = "foreign"
				default:
					f.mode = mode
				}
				observation, err := provider.ObservePullRequestRepair(t.Context(), value)
				if err == nil || observation.Matches || f.attempts != 1 {
					t.Fatal("false recovery", observation, err, f.attempts)
				}
			})
		}
	}
}
func TestPRRepairIntentBoundsAndAmbiguousPaths(t *testing.T) {
	f, provider := newRepairFixture(t, ProviderGitHub)
	initial := repairFixtureIntent(t, f, provider)
	for _, mode := range []string{"many", "bytes", "duplicate", "case", "prefix", "marker", "empty-delete", "nul", "noop"} {
		t.Run(mode, func(t *testing.T) {
			value := initial
			value.Changes = append([]PRRepairChange(nil), initial.Changes...)
			switch mode {
			case "many":
				value.Changes = make([]PRRepairChange, MaxPRRepairFiles+1)
			case "bytes":
				text := strings.Repeat("x", MaxPRRepairContentBytes+1)
				value.Changes[0].Content = &text
			case "duplicate":
				value.Changes[1].Path = "edit.txt"
			case "case":
				value.Changes[1].Path = "Edit.txt"
			case "prefix":
				value.Changes[1].Path = "edit.txt/child.txt"
			case "marker":
				value.Message = "Repair"
			case "empty-delete":
				value.Changes[0].PreviousBlob = ""
				value.Changes[0].Content = nil
			case "nul":
				text := "a\x00b"
				value.Changes[0].Content = &text
			case "noop":
				text := "before"
				value.Changes[0].Content = &text
			}
			result, err := provider.ApplyPullRequestRepair(t.Context(), value)
			if err == nil || result.MutationAttempted || f.attempts != 0 {
				t.Fatal(result, err, f.attempts)
			}
		})
	}
}

func TestPRRepairADOForeignScopeRefused(t *testing.T) {
	for _, mode := range []string{"foreign-project", "foreign-org"} {
		t.Run(mode, func(t *testing.T) {
			f, provider := newRepairFixture(t, ProviderADO)
			f.mode = mode
			if _, err := provider.InspectRepairPullRequest(t.Context(), f.repo(), "42"); !errors.Is(err, ErrPRRepair) {
				t.Fatal(err)
			}
		})
	}
}
