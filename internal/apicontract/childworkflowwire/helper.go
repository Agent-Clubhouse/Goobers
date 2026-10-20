package childworkflowwire

import (
	"errors"
	"strings"
	"unicode"
	"unicode/utf8"
)

// MaxHelperTaskBytes bounds task text before host compilation.
const MaxHelperTaskBytes = 16 << 10

// HelperRequest selects a task and an existing Goober within parent authority.
// Authority, credentials, destination branches and paths are never body data.
type HelperRequest struct {
	Task    string `json:"task"`
	Goober  string `json:"goober"`
	Harness string `json:"harness,omitempty"`
	Model   string `json:"model,omitempty"`
	Base    string `json:"base,omitempty"`
}

// ChildWorkflowHelperRequest uses the existing child start transport.
type ChildWorkflowHelperRequest struct {
	Helper HelperRequest `json:"helper"`
}

// Validate checks transport bounds, not authority or availability.
func (r HelperRequest) Validate() error {
	if strings.TrimSpace(r.Task) == "" || len(r.Task) > MaxHelperTaskBytes || !utf8.ValidString(r.Task) ||
		strings.IndexFunc(r.Task, func(c rune) bool { return unicode.IsControl(c) && c != '\n' && c != '\t' && c != '\r' }) >= 0 {
		return errors.New("helper task must contain nonempty UTF-8 text of at most 16384 bytes without control characters")
	}
	if !helperIdentifier(r.Goober, 128, true) || !helperIdentifier(r.Harness, 128, false) ||
		!helperIdentifier(r.Model, 256, false) || !helperIdentifier(r.Base, 128, false) {
		return errors.New("helper selections must be bounded identifiers without whitespace or control characters")
	}
	return nil
}

func helperIdentifier(value string, limit int, required bool) bool {
	return (!required || value != "") && len(value) <= limit && utf8.ValidString(value) &&
		strings.IndexFunc(value, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) < 0
}
