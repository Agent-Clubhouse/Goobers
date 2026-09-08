package main

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"

	"github.com/goobers/goobers/providers"
)

func TestADOSelectorCountBoundsAndPagination(t *testing.T) {
	for _, mode := range []string{"empty", "later-match", "incomplete", "stuck", "error"} {
		t.Run(mode, func(t *testing.T) {
			calls := 0
			client := &fakeADOSeedClient{list: func(req providers.ListWorkItemsRequest) ([]providers.WorkItem, error) {
				calls++
				if req.State != "open" || req.Limit != 100 || !slices.Equal(req.Labels, []string{"ready"}) {
					t.Fatalf("wrong selector query: %+v", req)
				}
				switch mode {
				case "error":
					return nil, errors.New("denied")
				case "stuck":
					req.PageInfo.HasNext = true
				case "incomplete":
					req.PageInfo.HasNext = true
					req.PageInfo.NextCursor = fmt.Sprint(calls)
				case "later-match":
					if calls == 1 {
						req.PageInfo.HasNext = true
						req.PageInfo.NextCursor = "250"
						return nil, nil
					}
					return []providers.WorkItem{{}}, nil
				}
				return nil, nil
			}}
			count, complete, err := adoSelectorCount(context.Background(), client, providers.RepositoryRef{}, []string{"ready"})
			wantError := mode == "error" || mode == "stuck"
			if (err != nil) != wantError || complete != (mode == "empty" || mode == "later-match") || calls > 10 {
				t.Fatalf("count=%d complete=%v err=%v calls=%d", count, complete, err, calls)
			}
			if mode == "later-match" && (count != 1 || calls != 2) {
				t.Fatalf("later page missed: count=%d calls=%d", count, calls)
			}
		})
	}
}
