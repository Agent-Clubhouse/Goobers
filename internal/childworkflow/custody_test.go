package childworkflow

import (
	"errors"
	"testing"

	"github.com/goobers/goobers/internal/triggerqueue"
)

func TestAcceptedCustodySurvivesNarrowedExecutionPolicy(t *testing.T) {
	for _, mode := range []string{"goober-removed", "opt-in-removed", "config-changed"} {
		t.Run(mode, func(t *testing.T) {
			service, resolver, _ := submissionFixture(t)
			original := resolver.current.Origin
			accepted, err := service.Submit(t.Context(), original, SubmissionRequest{InvocationKey: "child", Source: []byte(validProposal)})
			if err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "goober-removed":
				clear(resolver.current.Admission.Goobers)
			case "opt-in-removed":
				resolver.current.Admission.ExecutionRefusal = "current child opt-in removed"
			case "config-changed":
				resolver.current.Admission.ConfigDigest = digest([]byte("new config"))
				resolver.current.Origin.ConfigDigest = resolver.current.Admission.ConfigDigest
			}
			resolver.current.Origin.GrantID = "current-grant"
			resolver.current.Origin.AttemptID = "current-attempt"
			resolver.current.Origin.PolicyDigest = AuthorityPolicyDigest(resolver.current.Admission)
			bindSubmissionOrigin(t, service, resolver.current.Origin, original.GrantID)
			got, err := service.Get(t.Context(), resolver.current.Origin, "child")
			if err != nil || got.Envelope != accepted.Envelope {
				t.Fatal("accepted custody was recompiled under new permissions", got, err)
			}
			if _, err := service.Get(t.Context(), original, "child"); !errors.Is(err, ErrAuthorityUnavailable) {
				t.Fatal("stale attempt retained read rights", err)
			}

		})
	}
}

func TestRetainedPayloadTamperingFailsWithoutRecompilation(t *testing.T) {
	service, resolver, path := submissionFixture(t)
	accepted, err := service.Submit(t.Context(), resolver.current.Origin, SubmissionRequest{InvocationKey: "child", Source: []byte(validProposal)})
	if err != nil {
		t.Fatal(err)
	}
	db := submissionDB(t, path)
	if _, err := db.Exec(`UPDATE triggers SET payload=replace(CAST(payload AS TEXT),'generated-check','another-check') WHERE id=?`, accepted.Child.AcceptanceID); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Get(t.Context(), resolver.current.Origin, "child"); !errors.Is(err, triggerqueue.ErrConflict) {
		t.Fatal("unsealed payload accepted", err)
	}
}
