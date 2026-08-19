package mcpio

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
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
	if t.cfg.ReceiptFile == "" {
		return nil
	}
	full, err := t.resolveInWorkspace(t.cfg.ReceiptFile, true)
	if err != nil {
		return fmt.Errorf("resolve input inspection receipt file: %w", err)
	}
	file, err := os.OpenFile(full, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open input inspection receipt file: %w", err)
	}
	encodeErr := json.NewEncoder(file).Encode(receipt)
	closeErr := file.Close()
	if encodeErr != nil {
		return fmt.Errorf("write input inspection receipt: %w", encodeErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close input inspection receipt file: %w", closeErr)
	}
	return nil
}

// ReadInputInspectionReceipts reads the invocation receipt log. A missing file
// means no inspection tool was called.
func ReadInputInspectionReceipts(workspace, receiptFile string) ([]InputInspectionReceipt, error) {
	if receiptFile == "" {
		return nil, nil
	}
	full, err := resolveRooted(workspace, receiptFile, false)
	if err != nil {
		return nil, fmt.Errorf("resolve input inspection receipt file: %w", err)
	}
	file, err := os.Open(full)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("open input inspection receipt file: %w", err)
	}
	defer func() { _ = file.Close() }()

	var receipts []InputInspectionReceipt
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		var receipt InputInspectionReceipt
		if err := json.Unmarshal(scanner.Bytes(), &receipt); err != nil {
			return nil, fmt.Errorf("decode input inspection receipt: %w", err)
		}
		receipts = append(receipts, receipt)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read input inspection receipts: %w", err)
	}
	return receipts, nil
}

// ResetInputInspectionReceipts removes any prior invocation's receipt log.
func ResetInputInspectionReceipts(workspace, receiptFile string) error {
	if receiptFile == "" {
		return nil
	}
	full, err := resolveRooted(workspace, receiptFile, false)
	if err != nil {
		return fmt.Errorf("resolve input inspection receipt file: %w", err)
	}
	if err := os.Remove(full); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove input inspection receipt file: %w", err)
	}
	return nil
}
