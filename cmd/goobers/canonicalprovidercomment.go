package main

import (
	"fmt"
	"strings"

	"github.com/goobers/goobers/providers"
)

type canonicalProviderCommentSpec struct {
	noun   string
	list   func() ([]providers.Comment, error)
	create func(string) error
	update func(string, string) error
	remove func(string) error
	match  func([]providers.Comment) []providers.Comment
}

func reconcileCanonicalProviderComment(body string, spec canonicalProviderCommentSpec) error {
	comments, err := spec.list()
	if err != nil {
		return fmt.Errorf("list %s comments: %w", spec.noun, err)
	}
	matches := spec.match(comments)
	if len(matches) == 0 {
		if err := spec.create(body); err != nil {
			return fmt.Errorf("create %s comment: %w", spec.noun, err)
		}
	} else if err := spec.update(matches[0].ID, body); err != nil {
		return fmt.Errorf("update %s comment: %w", spec.noun, err)
	}

	comments, err = spec.list()
	if err != nil {
		return fmt.Errorf("relist %s comments: %w", spec.noun, err)
	}
	matches = spec.match(comments)
	if len(matches) == 0 {
		return fmt.Errorf("%s comment disappeared during reconciliation", spec.noun)
	}
	if providers.StripAttribution(matches[0].Body) != strings.TrimSpace(body) {
		if err := spec.update(matches[0].ID, body); err != nil {
			return fmt.Errorf("update canonical %s comment: %w", spec.noun, err)
		}
	}
	for _, duplicate := range matches[1:] {
		if err := spec.remove(duplicate.ID); err != nil {
			return fmt.Errorf("delete duplicate %s comment %s: %w", spec.noun, duplicate.ID, err)
		}
	}
	return nil
}
