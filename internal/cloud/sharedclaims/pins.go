package sharedclaims

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"

	"github.com/goobers/goobers/internal/cloud/acceptance"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/providers"
)

// ErrPolicyPinUnavailable refuses shared execution without the run's own
// control-held workflow pin. Child repository inheritance is not workflow
// authority: the child's workflow differs from its parent's. Supporting that
// requires the durable child custody record and frozen admission, not fallback.
var ErrPolicyPinUnavailable = errors.New("shared_claim_policy_pin_unavailable")

// ControlPins verifies the journal's policy identity against control.run_pin.
// A worker cannot replace a shared workflow snapshot with a local one by
// changing the journaled digest to match its replacement. No parent pin is used.
func ControlPins(read func(context.Context, string) (acceptance.RunPins, error)) func(context.Context, journal.RunIdentity) error {
	return func(ctx context.Context, id journal.RunIdentity) error {
		if read == nil {
			return ErrPolicyPinUnavailable
		}
		pin, err := read(ctx, id.RunID)
		if errors.Is(err, sql.ErrNoRows) {
			return httpapi.NewInterventionError(http.StatusConflict, "shared_claim_policy_pin_unavailable",
				fmt.Sprintf("run %s has no own start-time workflow pin; shared child policy is unavailable", id.RunID), ErrPolicyPinUnavailable)
		}
		if err != nil {
			return err
		}
		if pin.Workflow == "" || pin.WorkflowDigest == "" || pin.ConfigGeneration == "" ||
			pin.RunID != id.RunID || pin.Workflow != id.Workflow || pin.WorkflowVersion != id.WorkflowVersion ||
			pin.WorkflowDigest != id.WorkflowDigest || pin.Gaggle != id.Gaggle || pin.ConfigGeneration != id.ConfigGeneration {
			return errors.New("claim workflow identity disagrees with its control-held start pin")
		}
		if repo := id.WorkspaceRepository; repo != nil {
			pinned := providers.RepositoryRef{Provider: providers.ProviderKind(pin.RepoRef.Provider), URL: pin.RepoRef.BaseURL,
				Owner: pin.RepoRef.Owner, Project: pin.RepoRef.Project, Name: pin.RepoRef.Name}
			journaled := providers.RepositoryRef{Provider: providers.ProviderKind(repo.Provider), URL: repo.BaseURL,
				Owner: repo.Owner, Project: repo.Project, Name: repo.Name}
			if pinned.CanonicalKey() != journaled.CanonicalKey() {
				return errors.New("claim repository disagrees with its control-held start pin")
			}
		}
		return nil
	}
}
