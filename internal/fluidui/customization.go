// Package fluidui stores per-user Portal customizations for the Labs Fluid UI
// feature (#8338, #8415). A customization is a typed, validated presentation
// document: it never carries code, and it never affects authorization, gates,
// or instance configuration.
package fluidui

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// Version is the only customization schema version this build accepts.
const Version = "portal.customization/v1"

// Bounds keep a customization small enough to load on every Portal page.
const (
	MaxDocumentBytes = 16 << 10
	MaxPages         = 32
	MaxPanelsPerPage = 64
	MaxSavedViews    = 20
	MaxViewNameLen   = 64
	MaxViewRouteLen  = 512
	MaxRunPhases     = 16
)

// Densities accepted for Density.
const (
	DensityComfortable = "comfortable"
	DensityCompact     = "compact"
)

// Run-list sort orders accepted for RunList.Sort.
const (
	SortNewest       = "newest"
	SortOldest       = "oldest"
	SortFailingFirst = "failing-first"
)

// identifierPattern bounds page, panel and phase IDs to safe, lowercase slugs.
// The Portal ignores IDs it does not render, so new panels need no schema bump.
var identifierPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,63}$`)

// Customization is one user's Portal presentation document.
type Customization struct {
	Version    string                `json:"version"`
	Density    string                `json:"density,omitempty"`
	Pages      map[string]PageLayout `json:"pages,omitempty"`
	RunList    *RunList              `json:"runList,omitempty"`
	SavedViews []SavedView           `json:"savedViews,omitempty"`
}

// PageLayout hides and orders the panels of one Portal page.
type PageLayout struct {
	Hidden []string `json:"hidden,omitempty"`
	Order  []string `json:"order,omitempty"`
}

// RunList customizes the runs page default sort and phase filter.
type RunList struct {
	Sort   string   `json:"sort,omitempty"`
	Phases []string `json:"phases,omitempty"`
}

// SavedView is a named Portal route (hash fragment) the user can jump to.
type SavedView struct {
	Name  string `json:"name"`
	Route string `json:"route"`
}

// ValidationError names the offending field so clients can point at it.
type ValidationError struct {
	Field   string `json:"field"`
	Message string `json:"message"`
}

func (e *ValidationError) Error() string { return e.Field + ": " + e.Message }

func invalid(field, format string, args ...any) error {
	return &ValidationError{Field: field, Message: fmt.Sprintf(format, args...)}
}

// Decode strictly parses and validates a customization document. Unknown
// fields, trailing data, and oversized documents are rejected.
func Decode(data []byte) (Customization, error) {
	if len(data) > MaxDocumentBytes {
		return Customization{}, invalid("$", "document exceeds %d bytes", MaxDocumentBytes)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var doc Customization
	if err := decoder.Decode(&doc); err != nil {
		return Customization{}, invalid("$", "malformed customization: %v", err)
	}
	if decoder.More() {
		return Customization{}, invalid("$", "trailing data after customization")
	}
	if err := doc.Validate(); err != nil {
		return Customization{}, err
	}
	return doc, nil
}

// Validate reports the first schema violation in doc.
func (doc Customization) Validate() error {
	if doc.Version != Version {
		return invalid("version", "must be %q", Version)
	}
	switch doc.Density {
	case "", DensityComfortable, DensityCompact:
	default:
		return invalid("density", "must be %q or %q", DensityComfortable, DensityCompact)
	}
	if err := validatePages(doc.Pages); err != nil {
		return err
	}
	if doc.RunList != nil {
		switch doc.RunList.Sort {
		case "", SortNewest, SortOldest, SortFailingFirst:
		default:
			return invalid("runList.sort", "must be %q, %q or %q", SortNewest, SortOldest, SortFailingFirst)
		}
		if err := validateIDs("runList.phases", doc.RunList.Phases, MaxRunPhases); err != nil {
			return err
		}
	}
	return validateSavedViews(doc.SavedViews)
}

func validatePages(pages map[string]PageLayout) error {
	if len(pages) > MaxPages {
		return invalid("pages", "at most %d pages", MaxPages)
	}
	for page, layout := range pages {
		if !identifierPattern.MatchString(page) {
			return invalid("pages", "page ID %q is not a lowercase slug", page)
		}
		if err := validateIDs("pages."+page+".hidden", layout.Hidden, MaxPanelsPerPage); err != nil {
			return err
		}
		if err := validateIDs("pages."+page+".order", layout.Order, MaxPanelsPerPage); err != nil {
			return err
		}
	}
	return nil
}

func validateIDs(field string, ids []string, limit int) error {
	if len(ids) > limit {
		return invalid(field, "at most %d entries", limit)
	}
	seen := make(map[string]bool, len(ids))
	for i, id := range ids {
		if !identifierPattern.MatchString(id) {
			return invalid(fmt.Sprintf("%s[%d]", field, i), "%q is not a lowercase slug", id)
		}
		if seen[id] {
			return invalid(fmt.Sprintf("%s[%d]", field, i), "%q is repeated", id)
		}
		seen[id] = true
	}
	return nil
}

func validateSavedViews(views []SavedView) error {
	if len(views) > MaxSavedViews {
		return invalid("savedViews", "at most %d saved views", MaxSavedViews)
	}
	names := make(map[string]bool, len(views))
	for i, view := range views {
		field := fmt.Sprintf("savedViews[%d]", i)
		name := strings.TrimSpace(view.Name)
		if name == "" || len(name) > MaxViewNameLen || name != view.Name {
			return invalid(field+".name", "must be 1-%d characters without surrounding spaces", MaxViewNameLen)
		}
		if names[name] {
			return invalid(field+".name", "%q is repeated", name)
		}
		names[name] = true
		if err := validateViewRoute(view.Route); err != nil {
			return invalid(field+".route", "%v", err)
		}
	}
	return nil
}

// validateViewRoute admits only in-app hash routes, so a saved view can never
// navigate off the Portal or inject a script URL.
func validateViewRoute(route string) error {
	if len(route) > MaxViewRouteLen {
		return fmt.Errorf("at most %d characters", MaxViewRouteLen)
	}
	if !strings.HasPrefix(route, "#/") {
		return errors.New(`must be a Portal route starting with "#/"`)
	}
	for _, r := range route {
		if r < 0x21 || r == 0x7f || r == '<' || r == '>' || r == '"' || r == '\\' {
			return errors.New("contains a disallowed character")
		}
	}
	return nil
}
