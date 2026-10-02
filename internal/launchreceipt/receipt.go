// Package launchreceipt records prepared launches, never model-authored output.
// These private receipts are not proof that a pod started or a policy was enforced.
package launchreceipt

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
)

// MaxBytes bounds one receipt body.
const MaxBytes = 8192

// MaxTTL bounds controller launch authority to two minutes.
const MaxTTL = 2 * time.Minute

// TokenPrefix separates launch grants from all existing bearer kinds.
const TokenPrefix = "goobers-launch."

// ErrInvalid reports malformed or unauthorized launch evidence.
var ErrInvalid = errors.New("invalid launch receipt or grant")

// ErrUsed reports an already consumed canonical attempt.
var ErrUsed = errors.New("launch receipt already consumed")

// Binding comes from the durable controller start event, not the invocation's
// result or a stage pod's journal writes. Empty pins mean unknown legacy pins.
type Binding struct {
	RunID          string               `json:"runId"`
	Stage          string               `json:"stage"`
	Branch         int                  `json:"branch"`
	StartedSeq     uint64               `json:"startedSeq"`
	AttemptID      string               `json:"attemptId"`
	Number         int                  `json:"number"`
	Class          journal.AttemptClass `json:"class,omitempty"`
	Review         bool                 `json:"review"`
	WorkflowDigest string               `json:"workflowDigest,omitempty"`
	GooberDigest   string               `json:"gooberDigest,omitempty"`
}

// RemoteFacts is an explicit, bounded allowlist of control-plane observations.
// Fingerprints conceal configuration strings (image references and selectors).
// No env, command, annotation, mount source, prompt, token, or host path belongs here.
type RemoteFacts struct {
	ExecutionKitDigest           string `json:"executionKitDigest,omitempty"`
	Source                       string `json:"source"`
	ImageReferenceDigest         string `json:"imageReferenceDigest"`
	SelectorDigest               string `json:"selectorDigest"`
	HostNetwork                  bool   `json:"hostNetwork"`
	HostPID                      bool   `json:"hostPID"`
	HostIPC                      bool   `json:"hostIPC"`
	AutomountServiceAccountToken *bool  `json:"automountServiceAccountToken,omitempty"`
	RunAsNonRoot                 *bool  `json:"runAsNonRoot,omitempty"`
	Privileged                   *bool  `json:"privileged,omitempty"`
	ReadOnlyRootFilesystem       *bool  `json:"readOnlyRootFilesystem,omitempty"`
	AllowPrivilegeEscalation     *bool  `json:"allowPrivilegeEscalation,omitempty"`
	Seccomp                      string `json:"seccomp"`
	DropAllCapabilities          bool   `json:"dropAllCapabilities"`
	ContainerCount               int    `json:"containerCount"`
	WritableMountCount           int    `json:"writableMountCount"`
	NetworkEnforcement           string `json:"networkEnforcement"`
	SandboxEnforcement           string `json:"sandboxEnforcement"`
	ResolvedModel                string `json:"resolvedModel"`
	ResolvedEffort               string `json:"resolvedEffort"`
}

// Receipt holds only prepared facts; it does not assert successful launch.
type Receipt struct {
	Version int         `json:"version"`
	Binding Binding     `json:"binding"`
	Facts   RemoteFacts `json:"facts"`
	Local   *LocalFacts `json:"local,omitempty"`
}

// Grant binds both canonical identity and all receipt bytes. Changing even an
// allowed fact needs fresh controller authority; possession cannot widen it.
type Grant struct {
	AttemptID string `json:"attemptId"`
	Digest    string `json:"digest"`
	Expires   int64  `json:"expires"`
}

// Digest fingerprints configuration without persisting its raw strings.
func Digest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// ValidDigest accepts the repository SHA-256 pin spellings.
func ValidDigest(s string) bool {
	b, err := hex.DecodeString(strings.TrimPrefix(s, "sha256:"))
	return err == nil && len(b) == sha256.Size
}

// Encode validates the bounded allowlist and returns canonical grant-bound bytes.
func (r Receipt) Encode() ([]byte, error) {
	if r.Version != 1 || !r.Binding.valid() || !r.validFacts() {
		return nil, ErrInvalid
	}
	raw, err := json.Marshal(r)
	if err != nil || len(raw) > MaxBytes {
		return nil, ErrInvalid
	}
	return raw, nil
}

func (b Binding) valid() bool {
	if !apiv1.ValidRunID(b.RunID) || !apiv1.ValidRunID(b.Stage) || len(b.RunID) > 256 || len(b.Stage) > 256 ||
		b.Branch < 0 || b.StartedSeq == 0 || b.Number < 1 || b.AttemptID != journal.StageAttemptID(b.RunID, b.Branch, b.Stage, b.StartedSeq) ||
		(b.Class != "" && b.Class != journal.AttemptInfra && b.Class != journal.AttemptPolicy && b.Class != journal.AttemptHuman) ||
		(b.WorkflowDigest != "" && !ValidDigest(b.WorkflowDigest)) || (b.GooberDigest != "" && !ValidDigest(b.GooberDigest)) {
		return false
	}
	return true
}

func (f RemoteFacts) valid() bool {
	if f.Source != "control-plane-prepared" || (f.ExecutionKitDigest != "" && !ValidDigest(f.ExecutionKitDigest)) || !ValidDigest(f.ImageReferenceDigest) || !ValidDigest(f.SelectorDigest) ||
		f.ContainerCount < 1 || f.ContainerCount > 32 || f.WritableMountCount < 0 || f.WritableMountCount > 128 ||
		(f.Seccomp != "unknown" && f.Seccomp != "runtime-default" && f.Seccomp != "unconfined" && f.Seccomp != "localhost") ||
		f.NetworkEnforcement != "unknown" || f.SandboxEnforcement != "unknown" || f.ResolvedModel != "unknown" || f.ResolvedEffort != "unknown" {
		return false
	}
	return true
}

func (r Receipt) validFacts() bool {
	if r.Local != nil {
		return r.Facts == (RemoteFacts{}) && r.Local.valid()
	}
	return r.Facts.valid()
}
