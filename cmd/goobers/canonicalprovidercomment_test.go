package main

import (
	"errors"
	"reflect"
	"testing"

	"github.com/goobers/goobers/providers"
)

func TestReconcileCanonicalProviderComment(t *testing.T) {
	const body = "<!-- marker -->\ncurrent"

	t.Run("create then repair attributed drift and delete duplicates in order", func(t *testing.T) {
		stale, err := providers.StampAttribution(
			"<!-- marker -->\nstale",
			providers.Attribution{Gaggle: "gaggle", Workflow: "workflow", Task: "task", Goober: "goober", Run: "run"},
			"comment-update",
		)
		if err != nil {
			t.Fatal(err)
		}
		lists := [][]providers.Comment{
			nil,
			{
				{ID: "canonical", Body: stale},
				{ID: "duplicate-1", Body: body},
				{ID: "duplicate-2", Body: body},
			},
		}
		var operations []string
		spec := canonicalProviderCommentSpec{
			noun: "test",
			list: func() ([]providers.Comment, error) {
				operations = append(operations, "list")
				comments := lists[0]
				lists = lists[1:]
				return comments, nil
			},
			create: func(got string) error {
				operations = append(operations, "create "+got)
				return nil
			},
			update: func(id, got string) error {
				operations = append(operations, "update "+id+" "+got)
				return nil
			},
			remove: func(id string) error {
				operations = append(operations, "remove "+id)
				return nil
			},
			match: func(comments []providers.Comment) []providers.Comment {
				return comments
			},
		}

		if err := reconcileCanonicalProviderComment(body, spec); err != nil {
			t.Fatal(err)
		}
		want := []string{
			"list",
			"create " + body,
			"list",
			"update canonical " + body,
			"remove duplicate-1",
			"remove duplicate-2",
		}
		if !reflect.DeepEqual(operations, want) {
			t.Fatalf("operations = %q, want %q", operations, want)
		}
	})

	t.Run("update existing canonical without drift repair", func(t *testing.T) {
		lists := [][]providers.Comment{
			{{ID: "canonical", Body: "<!-- marker -->\nold"}},
			{{ID: "canonical", Body: body}},
		}
		var operations []string
		spec := canonicalProviderCommentSpec{
			noun: "test",
			list: func() ([]providers.Comment, error) {
				operations = append(operations, "list")
				comments := lists[0]
				lists = lists[1:]
				return comments, nil
			},
			create: func(string) error {
				t.Fatal("unexpected create")
				return nil
			},
			update: func(id, got string) error {
				operations = append(operations, "update "+id+" "+got)
				return nil
			},
			remove: func(string) error {
				t.Fatal("unexpected remove")
				return nil
			},
			match: func(comments []providers.Comment) []providers.Comment {
				return comments
			},
		}

		if err := reconcileCanonicalProviderComment(body, spec); err != nil {
			t.Fatal(err)
		}
		want := []string{"list", "update canonical " + body, "list"}
		if !reflect.DeepEqual(operations, want) {
			t.Fatalf("operations = %q, want %q", operations, want)
		}
	})

	t.Run("canonical disappears after relist", func(t *testing.T) {
		listCount := 0
		spec := canonicalProviderCommentSpec{
			noun: "test",
			list: func() ([]providers.Comment, error) {
				listCount++
				return nil, nil
			},
			create: func(string) error { return nil },
			update: func(string, string) error {
				t.Fatal("unexpected update")
				return nil
			},
			remove: func(string) error {
				t.Fatal("unexpected remove")
				return nil
			},
			match: func(comments []providers.Comment) []providers.Comment {
				return comments
			},
		}

		err := reconcileCanonicalProviderComment(body, spec)
		if got, want := err, errors.New("test comment disappeared during reconciliation"); got == nil || got.Error() != want.Error() {
			t.Fatalf("error = %v, want %v", got, want)
		}
		if listCount != 2 {
			t.Fatalf("list calls = %d, want 2", listCount)
		}
	})
}
