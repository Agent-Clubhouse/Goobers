package workspacerevision

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

func testRevision() *apiv1.WorkspaceRevision {
	base := apiv1.RepositoryIdentity{Provider: apiv1.ProviderGitHub, Owner: "org", Name: "base"}
	return &apiv1.WorkspaceRevision{
		Repository: base, CommitSHA: strings.Repeat("a", 40), SourceRef: "refs/heads/topic",
		SourceID: "123", BaseRepository: &base, BaseSHA: strings.Repeat("b", 40),
	}
}

func requireCode(t *testing.T, err error, code string) {
	t.Helper()
	var typed *Error
	wantNonRetryable := code != CodeAcquisition
	if !errors.As(err, &typed) || typed.Code != code || typed.NonRetryable() != wantNonRetryable {
		t.Fatalf("error = %v, want %s with NonRetryable=%t", err, code, wantNonRetryable)
	}
}

func TestAccept(t *testing.T) {
	candidate := testRevision()
	accepted, err := Accept(nil, candidate, true, true)
	if err != nil || !reflect.DeepEqual(accepted, candidate) || accepted == candidate ||
		accepted.BaseRepository == candidate.BaseRepository {
		t.Fatalf("establishment must deep copy: accepted=%+v err=%v", accepted, err)
	}
	candidate.BaseRepository.Name = "mutated"
	candidate.SourceRef = "mutated"
	if accepted.BaseRepository.Name != "base" || accepted.SourceRef != "refs/heads/topic" {
		t.Fatal("accepted binding aliases producer state")
	}
	for _, tc := range []struct {
		name          string
		current       *apiv1.WorkspaceRevision
		candidate     *apiv1.WorkspaceRevision
		deterministic bool
		success       bool
		code          string
	}{
		{"legacy", nil, nil, false, true, ""},
		{"absent preserves", accepted, nil, true, true, ""},
		{"identical", accepted, accepted.DeepCopy(), true, true, ""},
		{"agent cannot establish", nil, testRevision(), false, true, CodeUnauthorized},
		{"agent cannot reemit", accepted, accepted.DeepCopy(), false, true, CodeUnauthorized},
		{"failed agent rejected", nil, testRevision(), false, false, CodeUnauthorized},
		{"failure ignored", nil, testRevision(), true, false, ""},
		{"failure preserves", accepted, candidate, true, false, ""},
		{"replacement conflict", accepted, candidate, true, true, CodeConflict},
		{"invalid SHA", nil, &apiv1.WorkspaceRevision{Repository: accepted.Repository, CommitSHA: "main"}, true, true, CodeInvalid},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Accept(tc.current, tc.candidate, tc.deterministic, tc.success)
			if tc.code != "" {
				requireCode(t, err, tc.code)
			} else if err != nil {
				t.Fatal(err)
			}
			if got != tc.current {
				t.Fatal("no-op or rejection changed established state")
			}
		})
	}
}

func TestAcceptEveryFieldIsImmutable(t *testing.T) {
	current := testRevision()
	for _, mutate := range []struct {
		name string
		fn   func(*apiv1.WorkspaceRevision)
	}{
		{"commit", func(r *apiv1.WorkspaceRevision) { r.CommitSHA = strings.Repeat("c", 40) }},
		{"source ref", func(r *apiv1.WorkspaceRevision) { r.SourceRef = "new" }},
		{"source id", func(r *apiv1.WorkspaceRevision) { r.SourceID = "124" }},
		{"source repository", func(r *apiv1.WorkspaceRevision) { r.Repository.Name = "fork" }},
		{"base repository", func(r *apiv1.WorkspaceRevision) { r.BaseRepository.Name = "other" }},
		{"base commit", func(r *apiv1.WorkspaceRevision) { r.BaseSHA = strings.Repeat("c", 40) }},
		{"base absent", func(r *apiv1.WorkspaceRevision) { r.BaseRepository = nil }},
	} {
		t.Run(mutate.name, func(t *testing.T) {
			candidate := current.DeepCopy()
			mutate.fn(candidate)
			_, err := Accept(current, candidate, true, true)
			requireCode(t, err, CodeConflict)
		})
	}
}

func TestErrorTaxonomy(t *testing.T) {
	cause := errors.New("object missing")
	for _, tc := range []struct{ code, wire string }{
		{CodeInvalid, "workspace_revision_invalid"},
		{CodeUnauthorized, "workspace_revision_unauthorized"},
		{CodeConflict, "workspace_revision_conflict"},
		{CodeAcquisition, "workspace_revision_acquisition"},
		{CodeObjectType, "workspace_revision_object_type"},
		{CodeSHAMismatch, "workspace_revision_sha_mismatch"},
	} {
		err := &Error{Code: tc.code, Message: "refused", Cause: cause}
		requireCode(t, err, tc.wire)
		if !errors.Is(err, cause) || err.Error() != tc.wire+": refused" || err.StageErrorCode() != tc.wire {
			t.Fatalf("typed error lost detail: %v", err)
		}
	}
}
