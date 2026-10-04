package interactiveaccess

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"slices"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/journal"
)

// RestartAuthorityInputName is reserved for host-authenticated human authority.
// It carries no bearer or provider credential and grants nothing by itself:
// execution must recheck the current gaggle policy before each effect.
const RestartAuthorityInputName = "operator-restart-authority"
const maxRestartAuthorityBytes = 64 << 10

// RestartAuthority preserves the identity that accepted a particular epoch.
// Issuer and subject remain separate, including when either contains a colon.
// Group claims are the verified admission snapshot; policy revocation remains
// live. Display names, pod scopes and transport tokens are deliberately absent.
type RestartAuthority struct {
	Version           int            `json:"version"`
	EpochID           string         `json:"epochId"`
	SourceRunID       string         `json:"sourceRunId"`
	SourceTerminalSeq uint64         `json:"sourceTerminalSeq"`
	Gaggle            string         `json:"gaggle"`
	Stage             string         `json:"stage"`
	WorkflowDigest    string         `json:"workflowDigest"`
	GooberDigest      string         `json:"gooberDigest"`
	Issuer            string         `json:"issuer"`
	Subject           string         `json:"subject"`
	Roles             []httpapi.Role `json:"roles"`
	Groups            []string       `json:"groups"`
}

// NewRestartAuthority accepts only an already authenticated human principal.
// HTTP request bodies must never be decoded directly into this type.
func NewRestartAuthority(p httpapi.Principal, source journal.RunIdentity, epoch, stage string, sequence uint64) (RestartAuthority, error) {
	if p.ChildWorkflow != nil || len(p.Scopes) != 0 {
		return RestartAuthority{}, ErrDenied
	}
	a := RestartAuthority{Version: 1, EpochID: epoch, SourceRunID: source.RunID, SourceTerminalSeq: sequence, Gaggle: source.Gaggle, Stage: stage, WorkflowDigest: source.WorkflowDigest, GooberDigest: source.GooberDigest, Issuer: p.Issuer, Subject: p.Subject, Roles: slices.Clone(p.Roles), Groups: slices.Clone(p.Groups)}
	slices.Sort(a.Roles)
	a.Roles = slices.Compact(a.Roles)
	slices.Sort(a.Groups)
	a.Groups = slices.Compact(a.Groups)
	return a, a.validate()
}

// Principal reconstructs only the verified human claims retained at admission.
func (a RestartAuthority) Principal() httpapi.Principal {
	return httpapi.Principal{Issuer: a.Issuer, Subject: a.Subject, Roles: slices.Clone(a.Roles), Groups: slices.Clone(a.Groups)}
}

func (a RestartAuthority) validate() error {
	if a.Version != 1 || !apiv1.ValidRunID(a.EpochID) || !apiv1.ValidRunID(a.SourceRunID) || a.EpochID == a.SourceRunID || a.SourceTerminalSeq == 0 {
		return errors.New("invalid retained restart execution identity")
	}
	for _, field := range []struct {
		value string
		limit int
	}{{a.Gaggle, 256}, {a.Stage, 256}, {a.WorkflowDigest, 128}, {a.GooberDigest, 128}, {a.Issuer, 2048}, {a.Subject, 512}} {
		if !validText(field.value, field.limit) {
			return errors.New("invalid retained restart authority")
		}
	}
	principal := a.Principal()
	if !human(principal) || !principal.HasRole(httpapi.RoleOperate) {
		return errors.New("restart requires retained human operator authority")
	}
	return a.validateClaims()
}

func (a RestartAuthority) validateClaims() error {
	if len(a.Roles) > 3 || len(a.Groups) > 128 || !slices.IsSorted(a.Roles) || !slices.IsSorted(a.Groups) {
		return errors.New("restart authority claims are not bounded and canonical")
	}
	for i, role := range a.Roles {
		if (role != httpapi.RoleView && role != httpapi.RoleOperate && role != httpapi.RoleAdmin) || (i > 0 && a.Roles[i-1] == role) {
			return errors.New("invalid restart authority role")
		}
	}
	for i, group := range a.Groups {
		if !validText(group, 512) || (i > 0 && a.Groups[i-1] == group) {
			return errors.New("invalid restart authority group")
		}
	}
	return nil
}

// Marshal validates before producing the bounded immutable host input.
func (a RestartAuthority) Marshal() ([]byte, error) {
	if err := a.validate(); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(a)
	if err == nil && len(raw) > maxRestartAuthorityBytes {
		err = errors.New("restart authority exceeds retained size limit")
	}
	return raw, err
}

// ParseRestartAuthority requires the exact canonical host representation;
// this also refuses duplicate keys, unknown keys and alternate encodings.
func ParseRestartAuthority(raw []byte) (RestartAuthority, error) {
	var a RestartAuthority
	if len(raw) > maxRestartAuthorityBytes {
		return a, errors.New("restart authority exceeds retained size limit")
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(&a); err != nil {
		return a, err
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return a, errors.New("restart authority contains trailing JSON")
	}
	canonical, err := a.Marshal()
	if err != nil || !bytes.Equal(raw, canonical) {
		return a, errors.New("restart authority is not a canonical host record")
	}
	return a, nil
}

// LoadRestartAuthority verifies the artifact and its binding to the immutable
// continuation identity. A same-named ordinary workflow input is insufficient.
func LoadRestartAuthority(reader *journal.Reader, id journal.RunIdentity) (RestartAuthority, error) {
	if reader == nil {
		return RestartAuthority{}, errors.New("restart authority requires retained journal")
	}
	var found *journal.InputRef
	for i := range id.Inputs {
		if id.Inputs[i].Name != RestartAuthorityInputName {
			continue
		}
		if found != nil {
			return RestartAuthority{}, errors.New("duplicate restart authority input")
		}
		found = &id.Inputs[i]
	}
	if found == nil || found.Integrity != apiv1.IntegrityTrusted {
		return RestartAuthority{}, errors.New("trusted restart authority is missing")
	}
	raw, err := reader.ArtifactBytesBounded(found.Ref, maxRestartAuthorityBytes)
	if err != nil {
		return RestartAuthority{}, err
	}
	a, err := ParseRestartAuthority(raw)
	if err != nil {
		return RestartAuthority{}, err
	}
	if id.Child != nil || id.EngineDriven() || a.EpochID != id.RunID || a.SourceRunID != id.ContinuedFromRunID || a.SourceTerminalSeq != id.SourceTerminalSeq || a.Gaggle != id.Gaggle || a.Stage != id.RequestedTarget || a.WorkflowDigest != id.WorkflowDigest || a.GooberDigest != id.GooberDigest || id.Operator != a.Issuer+":"+a.Subject || found.Source != id.Operator {
		return RestartAuthority{}, errors.New("restart authority does not match retained execution")
	}
	return a, nil
}
