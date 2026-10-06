package providers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
)

type linkPageSender func(context.Context, string, string, interface{}) (*http.Response, error)

// pageContext is the per-page metadata a contextual callback receives.
type pageContext struct {
	Header  http.Header
	HasNext bool
}

func walkLinkPages(ctx context.Context, sender linkPageSender, endpoint string, onPage func([]byte, pageContext) error) error {
	next, err := withPerPage(endpoint, maxPerPage)
	if err != nil {
		return err
	}
	for next != "" {
		resp, err := sender(ctx, http.MethodGet, next, nil)
		if err != nil {
			return err
		}
		header := resp.Header.Clone()
		body, nextLink, err := readPage(resp, http.MethodGet, next)
		if err != nil {
			return err
		}
		if err := onPage(body, pageContext{Header: header, HasNext: nextLink != ""}); err != nil {
			if errors.Is(err, errStopPaging) {
				return nil
			}
			return err
		}
		next = nextLink
	}
	return nil
}

func walkPagedJSON[T any](ctx context.Context, pager restPager, endpoint, decodeError string, onPage func([]T) error) error {
	return pager.getAllPages(ctx, endpoint, func(page []byte) error {
		var items []T
		if err := json.Unmarshal(page, &items); err != nil {
			return fmt.Errorf("%s: %w", decodeError, err)
		}
		return onPage(items)
	})
}

func collectPagedJSON[T any](ctx context.Context, pager restPager, endpoint, decodeError string) ([]T, error) {
	var all []T
	err := walkPagedJSON(ctx, pager, endpoint, decodeError, func(items []T) error {
		all = append(all, items...)
		return nil
	})
	return all, err
}
