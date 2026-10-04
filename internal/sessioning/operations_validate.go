package sessioning

import (
	"errors"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/goobers/goobers/internal/workbench"
)

var sourceBindingPattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,62}[a-z0-9])?$`)

func operationText(text string, max int) bool {
	return text != "" && len(text) <= max && utf8.ValidString(text) && strings.TrimSpace(text) == text && strings.IndexFunc(text, unicode.IsControl) < 0
}

// ValidateBacklogRead limits source locators without interpreting provider IDs.
func ValidateBacklogRead(r BacklogReadRequest) error {
	if !sourceBindingPattern.MatchString(r.SourceBindingID) || !operationText(r.ID, 128) || (r.ExpectedSourceID != "" && !operationText(r.ExpectedSourceID, 512)) {
		return errors.New("invalid source binding or backlog item locator")
	}
	return nil
}

// ValidateBacklogList permits one bounded window. Zero uses provider default.
func ValidateBacklogList(r BacklogListRequest) error {
	if !sourceBindingPattern.MatchString(r.SourceBindingID) || r.Limit < 0 || r.Limit > workbench.MaxBacklogPageItems || (r.Cursor != "" && !operationText(r.Cursor, workbench.MaxBacklogCursorBytes)) {
		return errors.New("invalid source binding or backlog page")
	}
	return nil
}
