// Package blockedrecords is the runner-free core of the learned-dependency block
// records (#552): the record shape, its key and provider-lookup identity, and
// the projection onto the cycle detector. Where the records are stored is the
// caller's concern: the daemon keeps them in blocked.json over the
// scheduler-state plane, the cloud controller in PostgreSQL.
package blockedrecords

import (
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/blockedcycle"
	"github.com/goobers/goobers/internal/claimsclient"
	"github.com/goobers/goobers/providers"
)

// Record is one learned dependency block: the issue numbers the item was
// reported blocked on, plus provenance for a human inspecting the store.
type Record struct {
	Repository providers.RepositoryRef `json:"repository"`
	ItemID     string                  `json:"itemId"`
	Blockers   []string                `json:"blockers"`
	RunID      string                  `json:"runId"`
	Stage      string                  `json:"stage,omitempty"`
	Reason     string                  `json:"reason,omitempty"`
	RecordedAt time.Time               `json:"recordedAt"`
}

// Key identifies the record for one repository-scoped item.
func Key(repo providers.RepositoryRef, itemID string) string {
	return blockedcycle.RepositoryIdentity(repo) + "#" + url.PathEscape(itemID)
}

// ItemID is the record's item id; a legacy record without one is keyed by it.
func ItemID(key string, rec Record) string {
	if rec.ItemID != "" {
		return rec.ItemID
	}
	return key
}

// RepositoryEmpty reports a repository that carries no scoping.
func RepositoryEmpty(repo providers.RepositoryRef) bool { return blockedcycle.RepositoryEmpty(repo) }

// SameRepository reports whether a and b name one repository.
func SameRepository(a, b providers.RepositoryRef) bool {
	return a.Provider == b.Provider && a.Owner == b.Owner && a.Project == b.Project && a.Name == b.Name
}

// AppliesToRepository reports whether rec is scoped to repo. An unscoped record
// stays quarantined rather than applying to every repository.
func AppliesToRepository(rec Record, repo providers.RepositoryRef) bool {
	return !RepositoryEmpty(rec.Repository) && SameRepository(rec.Repository, repo)
}

// LookupID converts a recorded item id into the id a provider lookup expects.
// Item ids come from whatever the claim ledger used for the run's driving item,
// so a pr-remediation run records its claim name ("pr/955") while issue-driven
// runs record a bare "955". GitHub numbers issues and pull requests in one
// sequence and serves both at /issues/{number}, so stripping the prefix
// resolves the pull request (#971).
func LookupID(key string) string { return strings.TrimPrefix(key, claimsclient.PullRequestItemPrefix) }

// CycleRecords projects the recorded blocks onto the detector's input:
// repository-scoped, provider-lookup-normalized ids in deterministic key order,
// skipping records that carry no repository scoping.
func CycleRecords(recs map[string]Record) []blockedcycle.Record {
	keys := make([]string, 0, len(recs))
	for key := range recs {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	records := make([]blockedcycle.Record, 0, len(keys))
	for _, key := range keys {
		rec := recs[key]
		if RepositoryEmpty(rec.Repository) {
			continue
		}
		blockers := make([]string, len(rec.Blockers))
		for i, blockerID := range rec.Blockers {
			blockers[i] = LookupID(blockerID)
		}
		records = append(records, blockedcycle.Record{
			Repository: rec.Repository,
			ItemID:     LookupID(ItemID(key, rec)),
			Blockers:   blockers,
		})
	}
	return records
}

// FindCycle identifies the cycle containing the newly recorded item.
func FindCycle(recs map[string]Record, itemKey string) blockedcycle.Result {
	record, ok := recs[itemKey]
	if !ok || RepositoryEmpty(record.Repository) {
		return blockedcycle.Result{}
	}
	item := blockedcycle.Node{Repository: record.Repository, ItemID: LookupID(ItemID(itemKey, record))}
	return blockedcycle.Find(CycleRecords(recs), item)
}

// FindAllCycles enumerates every cycle currently present in recs, not just the
// one touching a single just-written key (#1405).
func FindAllCycles(recs map[string]Record) []blockedcycle.Result {
	return blockedcycle.FindAll(CycleRecords(recs))
}
