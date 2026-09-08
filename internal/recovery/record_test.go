package recovery

import (
	"strings"
	"testing"
	"time"
)

func TestRecordBindsRecoveryState(t *testing.T) {
	now := time.Date(2026, 9, 8, 0, 0, 0, 0, time.UTC)
	good := Record{Version: 1, RunID: "run-1", RepositoryKey: "github|||team|repo|", Ref: "refs/goobers/recovery/run-1", BaseSHA: strings.Repeat("a", 40), SnapshotSHA: strings.Repeat("b", 40), PatchDigest: "sha256:" + strings.Repeat("c", 64), CreatedAt: now, RetainUntil: now.Add(24 * time.Hour)}
	if err := good.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*Record){
		"version":                 func(r *Record) { r.Version++ },
		"branch injection":        func(r *Record) { r.Ref = "refs/heads/main" },
		"foreign owner":           func(r *Record) { r.RunID = "run-2" },
		"path traversal":          func(r *Record) { r.RunID = "../main" },
		"missing repository":      func(r *Record) { r.RepositoryKey = "" },
		"incomplete repository":   func(r *Record) { r.RepositoryKey = "team/repo" },
		"noncanonical repository": func(r *Record) { r.RepositoryKey = "github|||TEAM|repo|" },
		"missing ADO project":     func(r *Record) { r.RepositoryKey = "ado|||team|repo|" },
		"missing Gitea host":      func(r *Record) { r.RepositoryKey = "gitea|||team|repo|" },
		"bad base":                func(r *Record) { r.BaseSHA = "main" },
		"mixed object formats":    func(r *Record) { r.SnapshotSHA = strings.Repeat("b", 64) },
		"bad digest":              func(r *Record) { r.PatchDigest = "unverified" },
		"no recovery window":      func(r *Record) { r.RetainUntil = r.CreatedAt },
	} {
		t.Run(name, func(t *testing.T) {
			bad := good
			change(&bad)
			if err := bad.Validate(); err == nil {
				t.Fatal("invalid recovery record accepted")
			}
		})
	}
}
