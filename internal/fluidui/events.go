package fluidui

import (
	"bufio"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
)

// Aggregate learning (#8419) appends one privacy-safe event per customization
// transition. An event holds only the customization's shape (which controls
// differ from the standard UI), never panel IDs, phases, saved-view names or
// routes, and a salted pseudonymous user token used only to count distinct
// users. The salt stays under the store directory and is never reported.
const (
	eventApplied     = "applied"
	eventRevised     = "revised"
	eventReverted    = "reverted"
	eventUnsupported = "unsupported"
)

const (
	eventLogName        = "events.jsonl"
	rotatedEventLogName = "events.1.jsonl"
	eventSaltName       = "events.salt"
	eventSaltBytes      = 32
	// maxEventLogBytes bounds the active log; one rotated generation is kept,
	// so the learning log never exceeds about twice this size.
	maxEventLogBytes = 1 << 20
	// maxNeedLen and maxNeedWords keep unmet-need labels to short categories
	// so request text cannot be smuggled into the aggregate log.
	maxNeedLen   = 48
	maxNeedWords = 5
)

var (
	needSeparators = regexp.MustCompile(`[\s_]+`)
	needPattern    = regexp.MustCompile(`^[a-z][a-z0-9]*(-[a-z0-9]+)*$`)
)

// shapePages are the Portal page IDs a shape may name. Any other page ID is
// reported as "other" so an arbitrary user-chosen slug never reaches the log.
var shapePages = map[string]bool{
	"overview": true, "instance-detail": true, "workflows": true, "workflow": true, "goobers": true,
	"gaggle": true, "runs": true, "run": true, "errors": true, "insight": true, "cost": true,
	"labs": true, "work-items": true,
}

type event struct {
	Kind string `json:"kind"`
	// Shape is the user's shape after the transition; empty is the standard UI.
	Shape []string `json:"shape,omitempty"`
	// From is the shape before the transition.
	From []string  `json:"from,omitempty"`
	Need string    `json:"need,omitempty"`
	User string    `json:"user"`
	At   time.Time `json:"at"`
}

// shape lists the controls doc changes from the standard Portal, sorted.
// Closed enum values (density, run sort) are product-defined, so they are
// kept; free-form values (panel IDs, phases, saved views) are reduced to the
// control's name.
func (doc Customization) shape() []string {
	var out []string
	if doc.Density != "" && doc.Density != DensityComfortable {
		out = append(out, "density:"+doc.Density)
	}
	for page, layout := range doc.Pages {
		if !shapePages[page] {
			page = "other"
		}
		if len(layout.Hidden) > 0 {
			out = append(out, page+".hidden")
		}
		if len(layout.Order) > 0 {
			out = append(out, page+".order")
		}
	}
	if doc.RunList != nil {
		if doc.RunList.Sort != "" && doc.RunList.Sort != SortNewest {
			out = append(out, "runs.sort:"+doc.RunList.Sort)
		}
		if len(doc.RunList.Phases) > 0 {
			out = append(out, "runs.phase")
		}
	}
	if len(doc.SavedViews) > 0 {
		out = append(out, "savedViews")
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// documentShape is the shape of a stored document. The standard UI and an
// incompatible document both have no shape.
func documentShape(document json.RawMessage) []string {
	if len(document) == 0 || string(document) == "null" {
		return nil
	}
	doc, err := Decode(document)
	if err != nil {
		return nil
	}
	return doc.shape()
}

// transitionKind classifies a change from one shape to another. Returning to
// the standard UI, or undoing to a different shape, abandons the earlier
// shape and is a reversion.
func transitionKind(from, to []string, revert bool) string {
	switch {
	case len(from) == 0 && len(to) == 0:
		return ""
	case len(from) == 0:
		return eventApplied
	case len(to) == 0, revert && !slices.Equal(from, to):
		return eventReverted
	default:
		return eventRevised
	}
}

// recordTransition appends the learning event for a saved transition. The
// caller holds s.mu. Learning is advisory: a failed append never fails or
// rolls back the user's own customization write.
func (s *Store) recordTransition(user string, from json.RawMessage, current revision, revert bool) {
	before, after := documentShape(from), documentShape(current.Document)
	kind := transitionKind(before, after, revert)
	if kind == "" {
		return
	}
	token, err := s.userToken(user)
	if err != nil {
		return
	}
	_ = s.appendEvent(event{Kind: kind, Shape: after, From: before, User: token, At: current.UpdatedAt})
}

// RecordUnsupported records that user asked for something the customization
// schema cannot express (#8418). label must be a short category such as
// "dark-mode", never request text: it is normalized to a lowercase slug and
// rejected when longer than 48 characters or 5 words.
func (s *Store) RecordUnsupported(user, label string) error {
	need, err := normalizeNeed(label)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	token, err := s.userToken(user)
	if err != nil {
		return err
	}
	return s.appendEvent(event{Kind: eventUnsupported, Need: need, User: token, At: s.now()})
}

func normalizeNeed(label string) (string, error) {
	need := needSeparators.ReplaceAllString(strings.ToLower(strings.TrimSpace(label)), "-")
	if len(need) > maxNeedLen || !needPattern.MatchString(need) || strings.Count(need, "-") >= maxNeedWords {
		return "", invalid("need", "must be a category label of at most %d words and %d characters, not request text", maxNeedWords, maxNeedLen)
	}
	return need, nil
}

// userToken pseudonymizes user with the store's local salt. Tokens are kept
// only in the local log to count distinct users and never appear in a report.
func (s *Store) userToken(user string) (string, error) {
	salt, err := s.eventSalt()
	if err != nil {
		return "", err
	}
	mac := hmac.New(sha256.New, salt)
	_, _ = mac.Write([]byte(user))
	return hex.EncodeToString(mac.Sum(nil)[:16]), nil
}

func (s *Store) eventSalt() ([]byte, error) {
	if s.salt != nil {
		return s.salt, nil
	}
	path := filepath.Join(s.Dir, eventSaltName)
	salt, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		salt, err = s.createSalt(path)
	}
	if err != nil {
		return nil, fmt.Errorf("fluidui: read learning salt: %w", err)
	}
	if len(salt) != eventSaltBytes {
		return nil, fmt.Errorf("fluidui: learning salt is %d bytes, want %d", len(salt), eventSaltBytes)
	}
	s.salt = salt
	return salt, nil
}

func (s *Store) createSalt(path string) ([]byte, error) {
	salt := make([]byte, eventSaltBytes)
	if _, err := rand.Read(salt); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, fs.ErrExist) {
		// Another process created it first; use theirs so tokens agree.
		return os.ReadFile(path)
	}
	if err != nil {
		return nil, err
	}
	if _, err := file.Write(salt); err != nil {
		_ = file.Close()
		return nil, err
	}
	return salt, file.Close()
}

func (s *Store) appendEvent(e event) error {
	line, err := json.Marshal(e)
	if err != nil {
		return err
	}
	line = append(line, '\n')
	path := filepath.Join(s.Dir, eventLogName)
	if info, err := os.Stat(path); err == nil && info.Size()+int64(len(line)) > maxEventLogBytes {
		if err := os.Rename(path, filepath.Join(s.Dir, rotatedEventLogName)); err != nil {
			return err
		}
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	if _, err := file.Write(line); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

// readEvents returns the retained events, oldest log first. Lines that do not
// decode (for example, a torn final write) are skipped.
func readEvents(dir string) ([]event, error) {
	var events []event
	for _, name := range []string{rotatedEventLogName, eventLogName} {
		file, err := os.Open(filepath.Join(dir, name))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("fluidui: read learning log: %w", err)
		}
		scanner := bufio.NewScanner(file)
		for scanner.Scan() {
			var e event
			if json.Unmarshal(scanner.Bytes(), &e) == nil && e.User != "" {
				events = append(events, e)
			}
		}
		err = scanner.Err()
		_ = file.Close()
		if err != nil {
			return nil, fmt.Errorf("fluidui: read learning log: %w", err)
		}
	}
	return events, nil
}
