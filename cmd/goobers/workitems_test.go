package main

import (
	"bytes"
	"context"
	"io"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/readservice"
)

type fakeWorkItemReader struct {
	page          readservice.WorkItemPage
	detail        readservice.WorkItemDetail
	listOptions   readservice.WorkItemListOptions
	detailRequest [4]string
}

func (f *fakeWorkItemReader) WorkItems(_ context.Context, options readservice.WorkItemListOptions) (readservice.WorkItemPage, error) {
	f.listOptions = options
	return f.page, nil
}

func (f *fakeWorkItemReader) WorkItem(_ context.Context, provider, repository, kind, externalID string) (readservice.WorkItemDetail, error) {
	f.detailRequest = [4]string{provider, repository, kind, externalID}
	return f.detail, nil
}

type recordingCloser struct{ closed bool }

func (c *recordingCloser) Close() error { c.closed = true; return nil }

func TestRunWorkItemsUsesListWriterProjection(t *testing.T) {
	timestamp := time.Date(2026, time.September, 12, 17, 18, 19, 0, time.UTC)
	tests := []struct {
		name     string
		page     readservice.WorkItemPage
		wantJSON string
	}{
		{
			name:     "empty page preserves array shape",
			page:     readservice.WorkItemPage{Items: []readservice.WorkItemSummary{}},
			wantJSON: "{\"items\":[],\"hasMore\":false}\n",
		},
		{
			name: "partial page preserves required fields and continuation",
			page: readservice.WorkItemPage{
				Items: []readservice.WorkItemSummary{{
					Provider: "github", Kind: "issue", ExternalID: "83", ActionCount: 2,
					LastOperation: "comment", LastActionAt: timestamp, LastRunID: "run-7",
				}},
				HasMore: true,
			},
			wantJSON: "{\"items\":[{\"provider\":\"github\",\"kind\":\"issue\",\"externalId\":\"83\",\"actionCount\":2,\"lastOperation\":\"comment\",\"lastActionAt\":\"2026-09-12T17:18:19Z\",\"lastRunId\":\"run-7\"}],\"hasMore\":true}\n",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fake := &fakeWorkItemReader{page: test.page}
			closer := &recordingCloser{}
			var stdout, stderr bytes.Buffer
			var gotLayout instance.Layout
			var gotRebuild bool
			code := runWorkItemsWithReader(
				[]string{"--provider", " github ", "--kind", " issue ", "--limit", "17", "--json", "--rebuild", "/instance"},
				&stdout, &stderr,
				func(layout instance.Layout, rebuild bool) (workItemReader, io.Closer, error) {
					gotLayout, gotRebuild = layout, rebuild
					return fake, closer, nil
				},
			)
			if code != 0 || stderr.Len() != 0 {
				t.Fatalf("exit code = %d, stderr = %q; want 0 and empty", code, stderr.String())
			}
			if stdout.String() != test.wantJSON {
				t.Fatalf("stdout = %q, want exact writer projection %q", stdout.String(), test.wantJSON)
			}
			wantOptions := readservice.WorkItemListOptions{Provider: "github", Kind: "issue", Limit: 17}
			if fake.listOptions != wantOptions {
				t.Fatalf("WorkItems options = %+v, want %+v", fake.listOptions, wantOptions)
			}
			if gotLayout.Root != "/instance" || !gotRebuild || !closer.closed {
				t.Fatalf("reader lifecycle: root=%q rebuild=%t closed=%t; want /instance, true, true", gotLayout.Root, gotRebuild, closer.closed)
			}
		})
	}
}

func TestRunWorkItemsUsesDetailWriterProjection(t *testing.T) {
	timestamp := time.Date(2026, time.September, 12, 17, 18, 19, 0, time.UTC)
	fake := &fakeWorkItemReader{detail: readservice.WorkItemDetail{
		Provider: "github", Repository: "acme/widgets", Kind: "pr", ExternalID: "91",
		RelatedPullRequests: []readservice.RelatedWorkItem{},
		Actions: []readservice.WorkItemAction{{
			RunID: "run-8", Sequence: 3, Operation: "label", OccurredAt: timestamp,
		}},
	}}
	closer := &recordingCloser{}
	var stdout, stderr bytes.Buffer
	code := runWorkItemsWithReader(
		[]string{"--provider", "github", "--repository", "/acme/widgets/", "--kind", "pr", "--id", "91", "--json"},
		&stdout, &stderr,
		func(instance.Layout, bool) (workItemReader, io.Closer, error) { return fake, closer, nil },
	)
	if code != 0 || stderr.Len() != 0 {
		t.Fatalf("exit code = %d, stderr = %q; want 0 and empty", code, stderr.String())
	}
	want := "{\"provider\":\"github\",\"repository\":\"acme/widgets\",\"kind\":\"pr\",\"externalId\":\"91\",\"relatedPullRequests\":[],\"actions\":[{\"runId\":\"run-8\",\"sequence\":3,\"operation\":\"label\",\"occurredAt\":\"2026-09-12T17:18:19Z\"}],\"truncated\":false}\n"
	if stdout.String() != want {
		t.Fatalf("stdout = %q, want exact writer projection %q", stdout.String(), want)
	}
	if fake.detailRequest != [4]string{"github", "acme/widgets", "pr", "91"} {
		t.Fatalf("WorkItem request = %#v, want normalized identity", fake.detailRequest)
	}
	if !closer.closed {
		t.Fatal("reader was not closed")
	}
}
