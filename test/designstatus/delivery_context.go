package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"regexp"
	"strings"
)

type deliveryContext struct {
	BaseRevision  string   `json:"baseRevision"`
	ClosingIssues []string `json:"closingIssues"`
}

var revisionPattern = regexp.MustCompile(`^[0-9a-fA-F]{40}$`)
var localIssuePattern = regexp.MustCompile(`^#[1-9][0-9]*$`)

func checkDeliveryContext(file string, head []document) []string {
	data, err := os.ReadFile(file)
	if err != nil {
		return []string{fmt.Sprintf("delivery context: %v", err)}
	}
	context, err := parseDeliveryContext(data)
	if err != nil {
		return []string{fmt.Sprintf("delivery context: %v", err)}
	}
	if len(context.ClosingIssues) == 0 {
		return nil
	}
	base, err := loadBaseDocuments(context.BaseRevision)
	if err != nil {
		return []string{fmt.Sprintf("delivery context: %v", err)}
	}
	return checkDeliveryChanges(base, head, context.ClosingIssues)
}

func parseDeliveryContext(data []byte) (deliveryContext, error) {
	var result deliveryContext
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&result); err != nil {
		return result, err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return result, fmt.Errorf("expected exactly one context object")
	}
	if !revisionPattern.MatchString(result.BaseRevision) {
		return result, fmt.Errorf("baseRevision must be a full commit SHA")
	}
	if result.ClosingIssues == nil {
		return result, fmt.Errorf("closingIssues must be an explicit array")
	}
	for _, ref := range result.ClosingIssues {
		if !localIssuePattern.MatchString(ref) {
			return result, fmt.Errorf("invalid local closing issue %q", ref)
		}
	}
	return result, nil
}

func loadBaseDocuments(revision string) ([]document, error) {
	if !revisionPattern.MatchString(revision) {
		return nil, fmt.Errorf("invalid base revision")
	}
	output, err := exec.Command("git", "ls-tree", "-r", "--name-only", "-z", revision, "--", designRoot, adrRoot).Output()
	if err != nil {
		return nil, fmt.Errorf("read base tree %s (checkout must contain the PR base): %w", revision, err)
	}
	var documents []document
	for _, name := range strings.Split(string(output), "\x00") {
		if name == indexPath || !strings.EqualFold(path.Ext(name), ".md") {
			continue
		}
		body, err := exec.Command("git", "show", revision+":"+name).Output()
		if err != nil {
			return nil, fmt.Errorf("read base document %s: %w", name, err)
		}
		doc, err := parseDocumentBytes(name, body)
		if err != nil {
			return nil, err
		}
		documents = append(documents, doc)
	}
	return documents, nil
}
