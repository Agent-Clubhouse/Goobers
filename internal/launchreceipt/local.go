package launchreceipt

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"sync"
	"time"
)

// LocalFacts describes configuration prepared by this process. Unconfined
// same-UID execution cannot establish a protected receipt filesystem or a
// protected post-launch identity channel, so local integrity is unverified.
// Free-form configuration is fingerprinted rather than persisted verbatim.
type LocalFacts struct {
	Source                string `json:"source"`
	Integrity             string `json:"integrity"`
	Kind                  string `json:"kind"`
	HarnessDigest         string `json:"harnessDigest,omitempty"`
	HarnessVersionDigest  string `json:"harnessVersionDigest,omitempty"`
	RequestedModelDigest  string `json:"requestedModelDigest,omitempty"`
	RequestedEffortDigest string `json:"requestedEffortDigest,omitempty"`
	PreparedSandbox       string `json:"preparedSandbox"`
	SandboxEnforcement    string `json:"sandboxEnforcement"`
	NetworkEnforcement    string `json:"networkEnforcement"`
	DirectoryGrants       string `json:"directoryGrants"`
	ResolvedModel         string `json:"resolvedModel"`
	ResolvedEffort        string `json:"resolvedEffort"`
}

// PreparedLocal sets every unavailable observation explicitly. Callers may
// supply only fingerprints and a native mechanism they actually prepared.
func PreparedLocal(kind string) LocalFacts {
	return LocalFacts{Source: "runtime-prepared", Integrity: "unverified", Kind: kind,
		PreparedSandbox: "none", SandboxEnforcement: "unknown", NetworkEnforcement: "unknown",
		DirectoryGrants: "unknown", ResolvedModel: "unknown", ResolvedEffort: "unknown"}
}

func (f LocalFacts) valid() bool {
	if f.Source != "runtime-prepared" || f.Integrity != "unverified" ||
		(f.Kind != "agent" && f.Kind != "deterministic") ||
		(f.PreparedSandbox != "none" && f.PreparedSandbox != "seatbelt" && f.PreparedSandbox != "bwrap") ||
		f.SandboxEnforcement != "unknown" || f.NetworkEnforcement != "unknown" || f.DirectoryGrants != "unknown" ||
		f.ResolvedModel != "unknown" || f.ResolvedEffort != "unknown" {
		return false
	}
	for _, digest := range []string{f.HarnessDigest, f.HarnessVersionDigest, f.RequestedModelDigest, f.RequestedEffortDigest} {
		if digest != "" && !ValidDigest(digest) {
			return false
		}
	}
	return true
}

// LocalRecorder uses ephemeral, local-only authority for each prepared launch.
// It never loads reusable controller keys or produces a daemon bearer. Root is
// outside workspaces, but same-UID integrity remains explicitly unverified.
type LocalRecorder struct{ Root string }

// Record consumes one local-only grant and persists its immutable receipt.
func (l LocalRecorder) Record(ctx context.Context, receipt Receipt) error {
	if receipt.Local == nil {
		return ErrInvalid
	}
	raw, err := receipt.Encode()
	if err != nil {
		return err
	}
	var random [32]byte
	if _, err := rand.Read(random[:]); err != nil {
		return ErrInvalid
	}
	token := "goobers-local-launch." + hex.EncodeToString(random[:])
	now := time.Now()
	authority := &localAuthority{token: token, grant: Grant{AttemptID: receipt.Binding.AttemptID, Digest: Digest(raw), Expires: now.Add(MaxTTL).Unix()}, now: time.Now}
	store, err := NewStore(l.Root, authority)
	if err != nil {
		return err
	}
	return store.Accept(ctx, token, receipt)
}

type localAuthority struct {
	mu    sync.Mutex
	token string
	grant Grant
	now   func() time.Time
	used  bool
}

func (a *localAuthority) VerifyLaunchGrant(token string) (Grant, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.used || a.now().Unix() >= a.grant.Expires || subtle.ConstantTimeCompare([]byte(token), []byte(a.token)) != 1 {
		return Grant{}, ErrInvalid
	}
	a.used = true
	a.token = ""
	return a.grant, nil
}
