package readservice

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/triggerqueue"
)

func TestChildAdmissionProjectionShowsHoldEvidence(t *testing.T) {
	child := triggerqueue.ChildRecord{ChildID: "held", RunID: "child"}
	at := time.Date(2026, 10, 9, 15, 0, 0, 0, time.UTC)
	review := triggerqueue.ChildAdmissionReview{ChildID: "held", State: triggerqueue.ChildAdmissionPending, Reason: "out-of-distribution", Evidence: []byte(`{"scored":true,"modelId":"m","modelVersion":"workflowcluster.model/v1","featureVersion":"workflowcluster.features/v1","nearest":"c2","nearestMedoid":"x","nearestSize":3,"distance":0.42,"outOfDistribution":true,"humanApprovalRequired":true}`), RequestedAt: at}
	var sourceErr error
	service := &Local{sources: LocalSources{ChildAdmission: func(context.Context, triggerqueue.ChildIdentity) (triggerqueue.ChildAdmissionReview, error) {
		return review, sourceErr
	}}}
	got, err := service.childAdmission(t.Context(), child)
	if err != nil || got == nil || got.State != triggerqueue.ChildAdmissionPending || got.Novelty == nil || got.Novelty.Nearest != "c2" || !got.RequestedAt.Equal(at) || got.DecidedAt != nil {
		t.Fatalf("hold = %+v, %v", got, err)
	}
	review.Evidence = []byte(`{"scored":`)
	if got, err = service.childAdmission(t.Context(), child); err != nil || got == nil || got.Novelty != nil {
		t.Fatalf("invalid evidence became visible: %+v, %v", got, err)
	}
	sourceErr = sql.ErrNoRows
	if got, err = service.childAdmission(t.Context(), child); err != nil || got != nil {
		t.Fatalf("unheld child = %+v, %v", got, err)
	}
	sourceErr, review.ChildID = nil, "other"
	if _, err = service.childAdmission(t.Context(), child); err == nil {
		t.Fatal("foreign hold accepted")
	}
}
