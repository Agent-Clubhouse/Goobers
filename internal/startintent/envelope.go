// Package startintent normalizes ordinary workflow starts into the existing
// durable trigger ledger. Only the host captures execution pins and authority.
package startintent

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"

	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/triggerqueue"
)

// Kind distinguishes pinned ordinary starts from legacy named triggers.
const Kind = "goobers.workflow-start/v1"

// ErrInvalid identifies a definitive selector/configuration refusal before capture.
var ErrInvalid = errors.New("startintent: invalid ordinary selection")

// Request retains the caller's selection, independently of host-derived pins.
type Request struct {
	Workflow  string `json:"workflow"`
	Gaggle    string `json:"gaggle,omitempty"`
	SourceRun string `json:"sourceRun,omitempty"`
	Force     bool   `json:"force,omitempty"`
	PodScoped bool   `json:"podScoped,omitempty"`
	PodRunID  string `json:"podRunId,omitempty"`
}

// Target identifies the exact applied generation captured at acceptance.
type Target struct {
	Gaggle           string `json:"gaggle"`
	Workflow         string `json:"workflow"`
	ConfigGeneration string `json:"configGeneration"`
	WorkflowDigest   string `json:"workflowDigest"`
	GooberDigest     string `json:"gooberDigest"`
}

// Envelope is stored once. Repeated requests reuse its original target.
type Envelope struct {
	Source  *localscheduler.SourceTrigger `json:"source,omitempty"`
	Kind    string                        `json:"kind"`
	Request Request                       `json:"request"`
	Target  Target                        `json:"target"`
}

// Validate rejects unbounded and contradictory selectors before acceptance.
func (r Request) Validate() error {
	if !text(r.Workflow, 256, true) || !text(r.Gaggle, 256, false) || !text(r.SourceRun, 256, false) || !text(r.PodRunID, 256, false) {
		return errors.Join(ErrInvalid, errors.New("startintent: invalid request"))
	}
	if (r.SourceRun != "" && (r.Gaggle == "" || r.Force)) || (r.PodScoped && (r.Gaggle == "" || r.PodRunID == "" || r.Force)) || (!r.PodScoped && r.PodRunID != "") {
		return errors.Join(ErrInvalid, errors.New("startintent: incompatible request authority or options"))
	}
	return nil
}

// Validate checks host pin shape; archive compilation proves their contents.
func (t Target) Validate() error {
	for _, value := range []string{t.Gaggle, t.Workflow, t.ConfigGeneration, t.WorkflowDigest, t.GooberDigest} {
		if !text(value, 256, true) {
			return errors.New("startintent: incomplete execution pins")
		}
	}
	return nil
}

// Marshal returns the closed, bounded durable envelope.
func (e Envelope) Marshal() ([]byte, error) {
	if e.Kind != Kind || e.Request.Workflow != e.Target.Workflow || (e.Request.Gaggle != "" && e.Request.Gaggle != e.Target.Gaggle) {
		return nil, errors.New("startintent: target differs from request")
	}
	if err := errors.Join(e.Request.Validate(), e.Target.Validate()); err != nil {
		return nil, err
	}

	if e.Source != nil {
		if e.Request.Force || e.Request.SourceRun != "" || e.Request.PodScoped {
			return nil, errors.New("startintent: incompatible signal authority")
		}
		if err := e.Source.Validate(); err != nil {
			return nil, err
		}
	}
	return json.Marshal(e)
}

// Parse refuses unknown fields and additional documents.
func Parse(raw []byte) (Envelope, error) {
	var result Envelope
	if len(raw) == 0 || len(raw) > triggerqueue.MaxPayloadBytes {
		return result, errors.New("startintent: invalid envelope size")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&result); err != nil {
		return result, err
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return result, errors.New("startintent: trailing envelope")
	}
	_, err := result.Marshal()
	return result, err
}

func text(value string, limit int, required bool) bool {
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
