package mcpio

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/goobers/goobers/internal/pathutil"
)

// InputInspectionReceipt is tool-owned evidence that an invocation attempted
// to enumerate or inspect a materialized upstream input.
type InputInspectionReceipt struct {
	Tool        string `json:"tool"`
	Input       string `json:"input,omitempty"`
	InputDigest string `json:"inputDigest,omitempty"`
	Success     bool   `json:"success"`
	StartLine   int    `json:"startLine,omitempty"`
	EndLine     int    `json:"endLine,omitempty"`
	TotalLines  int    `json:"totalLines,omitempty"`
	Pattern     string `json:"pattern,omitempty"`
	MatchLines  []int  `json:"matchLines,omitempty"`
	Truncated   bool   `json:"truncated,omitempty"`
	Error       string `json:"error,omitempty"`
}

func (t *Toolset) recordInputInspection(receipt InputInspectionReceipt) error {
	return t.appendReceipt(t.cfg.ReceiptFile, "input inspection", receipt)
}

// appendReceipt appends one JSON line to the workspace-relative log rel; an
// empty rel means the harness did not ask for this log.
func (t *Toolset) appendReceipt(rel, what string, receipt any) error {
	if rel == "" {
		return nil
	}
	full, err := t.resolveInWorkspace(rel, true)
	if err != nil {
		return fmt.Errorf("resolve %s receipt file: %w", what, err)
	}
	file, err := os.OpenFile(full, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open %s receipt file: %w", what, err)
	}
	encodeErr := json.NewEncoder(file).Encode(receipt)
	closeErr := file.Close()
	if encodeErr != nil {
		return fmt.Errorf("write %s receipt: %w", what, encodeErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close %s receipt file: %w", what, closeErr)
	}
	return nil
}

// ReadInputInspectionReceipts reads the invocation receipt log. A missing file
// means no inspection tool was called.
func ReadInputInspectionReceipts(workspace, receiptFile string) ([]InputInspectionReceipt, error) {
	return readReceipts[InputInspectionReceipt](workspace, receiptFile, "input inspection")
}

// ResetInputInspectionReceipts removes any prior invocation's receipt log.
func ResetInputInspectionReceipts(workspace, receiptFile string) error {
	return resetReceipts(workspace, receiptFile, "input inspection")
}

// receiptDirMissing reports that the receipt log's directory does not exist,
// so no goobers-io server has run in this workspace and there is no log.
func receiptDirMissing(workspace, receiptFile string) bool {
	_, err := os.Lstat(filepath.Join(workspace, filepath.Dir(receiptFile)))
	return errors.Is(err, os.ErrNotExist)
}

func readReceipts[T any](workspace, receiptFile, what string) ([]T, error) {
	if receiptFile == "" || receiptDirMissing(workspace, receiptFile) {
		return nil, nil
	}
	full, err := pathutil.ResolveRootedPath(workspace, receiptFile, false)
	if err != nil {
		return nil, fmt.Errorf("resolve %s receipt file: %w", what, err)
	}
	file, err := os.Open(full)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("open %s receipt file: %w", what, err)
	}
	defer func() { _ = file.Close() }()

	var receipts []T
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		var receipt T
		if err := json.Unmarshal(scanner.Bytes(), &receipt); err != nil {
			return nil, fmt.Errorf("decode %s receipt: %w", what, err)
		}
		receipts = append(receipts, receipt)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read %s receipts: %w", what, err)
	}
	return receipts, nil
}

func resetReceipts(workspace, receiptFile, what string) error {
	if receiptFile == "" || receiptDirMissing(workspace, receiptFile) {
		return nil
	}
	full, err := pathutil.ResolveRootedPath(workspace, receiptFile, false)
	if err != nil {
		return fmt.Errorf("resolve %s receipt file: %w", what, err)
	}
	if err := os.Remove(full); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove %s receipt file: %w", what, err)
	}
	return nil
}
