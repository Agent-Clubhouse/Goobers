package sessioning

import (
	"bytes"
	"encoding/json"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// MaxRepairTargetBytes bounds human selection in messages and retained inputs.
const MaxRepairTargetBytes = 4096

// RepairRepository is an exact configured provider target, never a URL or token.
type RepairRepository struct {
	Provider string `json:"provider"`
	Owner    string `json:"owner"`
	Project  string `json:"project,omitempty"`
	Name     string `json:"name"`
}

// PRRepairTarget records an explicit human selection at the inspected head.
// It is intent, not a credential grant. Host tools recheck current permissions,
// source binding, native identities, head and writer custody before each effect.
type PRRepairTarget struct {
	SourceBindingID    string           `json:"sourceBindingId"`
	Repository         RepairRepository `json:"repository"`
	RepositorySourceID string           `json:"repositorySourceId"`
	ID                 string           `json:"id"`
	SourceID           string           `json:"sourceId"`
	ExpectedHeadSHA    string           `json:"expectedHeadSha"`
}

var (
	repairBinding  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
	repairNativeID = regexp.MustCompile(`^[1-9][0-9]{0,18}$`)
	repairUUID     = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
)

// ValidatePRRepairTarget validates only bounded identity syntax. It cannot prove
// that the human may repair this target or that it remains open and unchanged.
func ValidatePRRepairTarget(t *PRRepairTarget) error {
	if t == nil {
		return nil
	}
	if !repairBinding.MatchString(t.SourceBindingID) || !repairNativeID.MatchString(t.ID) || !repairNativeID.MatchString(t.SourceID) || !repairCoordinate(t.Repository.Owner) || !repairCoordinate(t.Repository.Name) {
		return ErrInvalidRequest
	}
	if (len(t.ExpectedHeadSHA) != 40 && len(t.ExpectedHeadSHA) != 64) || strings.Trim(t.ExpectedHeadSHA, "0123456789abcdef") != "" {
		return ErrInvalidRequest
	}
	switch t.Repository.Provider {
	case "github":
		if t.Repository.Project != "" || !repairNativeID.MatchString(t.RepositorySourceID) {
			return ErrInvalidRequest
		}
	case "ado":
		if !repairCoordinate(t.Repository.Project) || !repairUUID.MatchString(t.RepositorySourceID) {
			return ErrInvalidRequest
		}
	default:
		return ErrInvalidRequest
	}
	return nil
}

func repairCoordinate(s string) bool {
	return s != "" && s != "." && s != ".." && len(s) <= 256 && utf8.ValidString(s) && strings.TrimSpace(s) == s && !strings.ContainsAny(s, `/\\?#@:`) && strings.IndexFunc(s, unicode.IsControl) < 0
}

// CopyPRRepairTarget isolates caller-owned request pointers before acceptance.
func CopyPRRepairTarget(t *PRRepairTarget) *PRRepairTarget {
	if t == nil {
		return nil
	}
	copy := *t
	return &copy
}

// MarshalPRRepairTarget keeps absent selection byte-identical to old messages.
func MarshalPRRepairTarget(t *PRRepairTarget) ([]byte, error) {
	if err := ValidatePRRepairTarget(t); err != nil {
		return nil, err
	}
	if t == nil {
		return []byte{}, nil
	}
	raw, err := json.Marshal(t)
	if err != nil || len(raw) > MaxRepairTargetBytes {
		return nil, ErrInvalidRequest
	}
	return raw, nil
}

// ParsePRRepairTarget rejects unknown, duplicate, case-aliased and noncanonical
// durable fields; only an empty field denotes a legacy message with no target.
func ParsePRRepairTarget(raw []byte) (*PRRepairTarget, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	if len(raw) > MaxRepairTargetBytes {
		return nil, ErrInvalidRequest
	}
	var t PRRepairTarget
	if err := json.Unmarshal(raw, &t); err != nil {
		return nil, ErrInvalidRequest
	}
	canonical, err := MarshalPRRepairTarget(&t)
	if err != nil || !bytes.Equal(raw, canonical) {
		return nil, ErrInvalidRequest
	}
	return &t, nil
}

// MessageDigest preserves legacy text-only hashes. Selected targets use a
// separate domain and canonical payload so neither text nor identity can change.
func MessageDigest(text string, target *PRRepairTarget) string {
	if target == nil {
		return Digest([]byte(text))
	}
	raw, _ := json.Marshal(struct {
		Kind   string          `json:"kind"`
		Text   string          `json:"text"`
		Target *PRRepairTarget `json:"repairTarget"`
	}{"interactive-session-repair-input/v1", text, target})
	return Digest(raw)
}

// MessageContentBytes accounts for structured selection in page bounds.
func MessageContentBytes(m Message) int {
	raw, _ := json.Marshal(m.RepairTarget)
	return len(m.Text) + len(raw)
}
