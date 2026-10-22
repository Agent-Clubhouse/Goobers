package fanout

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"

	apischemas "github.com/goobers/goobers/api/schemas"
	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/handoffcheck"
	"github.com/goobers/goobers/internal/journal"
)

// MaxSelectionReasonBytes bounds one optional-branch selection reason.
const MaxSelectionReasonBytes = 512

type selectionDocument struct {
	SchemaVersion int `json:"schemaVersion"`
	Branches      []struct {
		Name   string `json:"name"`
		Reason string `json:"reason,omitempty"`
	} `json:"branches"`
}

// FreezeSelection verifies a journal-bound selection source and returns the
// normalized result recorded in Temporal history and parallel.started.
func FreezeSelection(
	def apiv1.Parallel,
	source journal.FanOutSourceRef,
	sourceData []byte,
	maxSourceBytes int64,
) (journal.ParallelSelection, error) {
	if err := validateSelectionSource(def, source, sourceData, maxSourceBytes); err != nil {
		return journal.ParallelSelection{}, err
	}
	schema, err := selectionSchema()
	if err != nil {
		return journal.ParallelSelection{}, err
	}
	if verdict := schema.Check(sourceData); !verdict.Valid {
		return journal.ParallelSelection{}, &SourceError{
			Reason: ReasonEnvelope,
			Detail: fmt.Sprintf(
				"document violates %s: %s",
				apischemas.FanOutSelection,
				summarizeSelectionIssues(verdict.Issues),
			),
		}
	}
	var document selectionDocument
	decoder := json.NewDecoder(bytes.NewReader(sourceData))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&document); err != nil {
		return journal.ParallelSelection{}, &SourceError{
			Reason: ReasonEnvelope, Detail: fmt.Sprintf("decode selection document: %v", err),
		}
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return journal.ParallelSelection{}, &SourceError{Reason: ReasonEnvelope, Detail: "trailing JSON value"}
	}
	chosen := make(map[string]string, len(document.Branches))
	declared := make(map[string]bool, len(def.Branches))
	for _, branch := range def.Branches {
		declared[branch.Name] = true
	}
	for _, entry := range document.Branches {
		if !declared[entry.Name] {
			return journal.ParallelSelection{}, selectionError("names unknown branch %q", entry.Name)
		}
		if _, duplicate := chosen[entry.Name]; duplicate {
			return journal.ParallelSelection{}, selectionError("names branch %q more than once", entry.Name)
		}
		if len(entry.Reason) > MaxSelectionReasonBytes {
			return journal.ParallelSelection{}, selectionError(
				"reason for branch %q exceeds %d bytes", entry.Name, MaxSelectionReasonBytes,
			)
		}
		chosen[entry.Name] = entry.Reason
	}
	integrity := source.Integrity
	if !integrity.Valid() {
		integrity = apiv1.IntegrityDerived
	}
	selection := journal.ParallelSelection{
		Source: journal.SelectionSource{
			Task: source.Task, Slot: source.Slot, Visit: source.Visit, Attempt: int32(source.Attempt),
		},
		SourceDigest: source.Digest,
		Integrity:    integrity,
		Selected:     []string{},
	}
	for _, branch := range def.Branches {
		reason, selected := chosen[branch.Name]
		if !branch.Optional || !selected {
			continue
		}
		selection.Selected = append(selection.Selected, branch.Name)
		if reason != "" {
			if selection.Reasons == nil {
				selection.Reasons = make(map[string]string)
			}
			selection.Reasons[branch.Name] = reason
		}
	}
	return selection, nil
}

func validateSelectionSource(
	def apiv1.Parallel,
	source journal.FanOutSourceRef,
	data []byte,
	maxSourceBytes int64,
) error {
	if def.Selection == nil {
		return selectionError("parallel %q has no selection declaration", def.Name)
	}
	switch {
	case def.Name == "":
		return selectionError("parallel name must not be empty")
	case source.Task != def.Selection.Source.Task || source.Slot != def.Selection.Source.Slot:
		return selectionError("source %s.%s differs from pinned selection source %s.%s",
			source.Task, source.Slot, def.Selection.Source.Task, def.Selection.Source.Slot)
	case source.Attempt > int(^uint32(0)>>1):
		return selectionError("source attempt %d exceeds int32", source.Attempt)
	case source.Size != int64(len(data)):
		return selectionError("source size %d does not match %d stored bytes", source.Size, len(data))
	case journal.Digest(data) != source.Digest:
		return &SourceError{Reason: ReasonDigestMismatch, Detail: fmt.Sprintf("source digest %s does not match stored bytes", source.Digest)}
	}
	if err := ValidateSourceBlobRef(source, maxSourceBytes); err != nil {
		return err
	}
	return nil
}

func summarizeSelectionIssues(issues []handoffcheck.Issue) string {
	parts := make([]string, 0, min(len(issues), 4))
	for i, issue := range issues {
		if i == 3 {
			parts = append(parts, fmt.Sprintf("and %d more", len(issues)-i))
			break
		}
		path := issue.Path
		if path == "" {
			path = "/"
		}
		parts = append(parts, path+": "+issue.Message)
	}
	return strings.Join(parts, "; ")
}

func selectionError(format string, args ...any) error {
	return &SourceError{Reason: ReasonEnvelope, Detail: fmt.Sprintf(format, args...)}
}

var (
	selectionSchemaOnce sync.Once
	selectionSchemaHost *handoffcheck.Schema
	selectionSchemaErr  error
)

func selectionSchema() (*handoffcheck.Schema, error) {
	selectionSchemaOnce.Do(func() {
		data, err := apischemas.FS.ReadFile(apischemas.FanOutSelection)
		if err != nil {
			selectionSchemaErr = fmt.Errorf("read embedded selection envelope: %w", err)
			return
		}
		selectionSchemaHost, selectionSchemaErr = handoffcheck.Compile(apischemas.FanOutSelection, "", data)
	})
	return selectionSchemaHost, selectionSchemaErr
}
