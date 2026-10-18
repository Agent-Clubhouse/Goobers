// Package enginestartintent gives explicit direct Temporal starts durable custody
// without changing their scheduler-bypass or dedupe-derived identity contract.
package enginestartintent

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"strings"

	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/triggerqueue"
)

// Kind is deliberately distinct from an ordinary scheduler-admitted start.
const Kind = "goobers.direct-engine-start/v1"

// Actor identifies same-root command admission; no remote request can supply it.
const Actor = "direct-engine-cli"

// Request pins the explicitly selected transport and execution options. Binding
// is a digest of credential selectors, never credential values. Directory fixes
// relative credential paths without changing the process working directory.
type Request struct {
	HostPort    string `json:"hostPort"`
	Namespace   string `json:"namespace"`
	TaskQueue   string `json:"taskQueue"`
	Gaggle      string `json:"gaggle"`
	Workflow    string `json:"workflow"`
	DedupeKey   string `json:"dedupeKey,omitempty"`
	LiveJournal bool   `json:"liveJournal,omitempty"`
	Binding     string `json:"binding"`
	Directory   string `json:"directory"`
}

// Envelope binds a closed request to its immutable canonical input attachment.
type Envelope struct {
	Kind             string  `json:"kind"`
	Request          Request `json:"request"`
	RunID            string  `json:"runId"`
	ConfigGeneration string  `json:"configGeneration"`
	InputDigest      string  `json:"inputDigest"`
}

// Validate refuses unbounded selectors and unsupported source shapes.
func (r Request) Validate() error {
	for _, value := range []string{r.HostPort, r.Namespace, r.TaskQueue, r.Gaggle, r.Workflow, r.Binding, r.Directory} {
		if !boundedText(value, 4096, true) {
			return errors.New("direct engine: invalid request selector")
		}
	}
	if !boundedText(r.DedupeKey, 256, false) || !filepath.IsAbs(r.Directory) || !strings.HasPrefix(r.Binding, "sha256:") || len(r.Binding) != 71 {
		return errors.New("direct engine: invalid dedupe or credential binding")
	}
	return nil
}

// Key retains existing per-endpoint/namespace/gaggle/workflow dedupe identity.
func (r Request) Key() string {
	raw, _ := json.Marshal([]string{r.HostPort, r.Namespace, engine.RunID(r.Gaggle, r.Workflow, r.DedupeKey)})
	return "direct-engine:" + strings.TrimPrefix(Digest(raw), "sha256:")
}

// Digest is the explicit algorithm-tagged identity of canonical input bytes.
func Digest(raw []byte) string {
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// Marshal checks host-derived identities before persistence.
func (e Envelope) Marshal() ([]byte, error) {
	if err := e.Request.Validate(); err != nil {
		return nil, err
	}
	if e.Kind != Kind || e.RunID != engine.RunID(e.Request.Gaggle, e.Request.Workflow, e.Request.DedupeKey) || !boundedText(e.ConfigGeneration, 256, true) || !strings.HasPrefix(e.InputDigest, "sha256:") || len(e.InputDigest) != 71 {
		return nil, errors.New("direct engine: invalid execution pins")
	}
	return json.Marshal(e)
}

// Parse refuses unknown fields and trailing JSON.
func Parse(raw []byte) (Envelope, error) {
	var e Envelope
	if err := decode(raw, triggerqueue.MaxPayloadBytes, &e); err != nil {
		return e, err
	}
	_, err := e.Marshal()
	return e, err
}

// Input verifies the entire stored snapshot before it can reach a provider.
func (e Envelope) Input(raw []byte) (engine.RunInput, error) {
	var in engine.RunInput
	if Digest(raw) != e.InputDigest {
		return in, errors.New("direct engine: input digest differs")
	}
	if err := decode(raw, triggerqueue.MaxDirectEngineInputBytes, &in); err != nil {
		return in, err
	}
	if in.RunID != e.RunID || in.Gaggle != e.Request.Gaggle || in.WorkflowName != e.Request.Workflow || in.ConfigGeneration != e.ConfigGeneration || in.LiveJournal != e.Request.LiveJournal || in.InstanceID == "" {
		return in, errors.New("direct engine: input identity differs")
	}
	return in, nil
}

func decode(raw []byte, limit int, into any) error {
	if len(raw) == 0 || len(raw) > limit {
		return errors.New("direct engine: invalid payload size")
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(into); err != nil {
		return err
	}
	if err := d.Decode(new(any)); !errors.Is(err, io.EOF) {
		return errors.New("direct engine: trailing payload")
	}
	return nil
}
func boundedText(value string, limit int, required bool) bool {
	if (required && value == "") || len(value) > limit || strings.TrimSpace(value) != value {
		return false
	}
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}
