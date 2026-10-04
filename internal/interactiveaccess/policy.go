// Package interactiveaccess authorizes human operations against one applied
// gaggle and explicitly selected credential sources. Automation grants do not
// participate in this authority plane.
package interactiveaccess

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/httpapi"
)

// ErrDenied is deliberately independent of whether a gaggle exists.
var ErrDenied = errors.New("interactive operation is not authorized")

// ErrCredentialUnavailable indicates that the explicitly selected target has no usable source.
var ErrCredentialUnavailable = errors.New("interactive credential is not configured for this target")

var actions = []apiv1.InteractiveAction{"session.create", "session.message", "backlog.read", "backlog.edit", "backlog.resolve", "repository.read", "run.intervene", "run.restartStage", "pr.repair", "source.proposeChange"}

func human(p httpapi.Principal) bool {
	return p.Subject != "" && p.Issuer != "" && !strings.HasPrefix(p.Issuer, "goobers/") && p.HasRole(httpapi.RoleView)
}

func membership(p httpapi.Principal, policy *apiv1.InteractiveAccessPolicy) (viewer, operator bool) {
	if !human(p) || policy == nil {
		return false, false
	}
	operator = p.HasRole(httpapi.RoleOperate) && matchesHuman(p, policy.Humans.Operators)
	// An operator grant implies visibility even for a view-only instance role.
	viewer = operator || matchesHuman(p, policy.Humans.Viewers) || matchesHuman(p, policy.Humans.Operators)
	return viewer, operator
}

func matchesHuman(p httpapi.Principal, grants []apiv1.InteractiveHumanGrant) bool {
	for _, grant := range grants {
		if grant.Issuer != p.Issuer {
			continue
		}
		if grant.Subject != "" && grant.Subject == p.Subject {
			return true
		}
		if grant.Group != "" && slices.Contains(p.Groups, grant.Group) {
			return true
		}
	}
	return false
}

func readAction(action apiv1.InteractiveAction) bool {
	return action == "backlog.read" || action == "repository.read"
}
func providerAction(action apiv1.InteractiveAction) bool {
	return readAction(action) || action == "backlog.edit" || action == "backlog.resolve" || action == "pr.repair" || action == "source.proposeChange"
}

func authorize(p httpapi.Principal, g *apiv1.Gaggle, action apiv1.InteractiveAction) error {
	if g == nil || g.Spec.InteractiveAccess == nil || !slices.Contains(actions, action) {
		return ErrDenied
	}
	policy := g.Spec.InteractiveAccess
	view, operate := membership(p, policy)
	if !view || (!readAction(action) && !operate) || !slices.Contains(policy.Actions, action) {
		return ErrDenied
	}
	return nil
}

func validText(value string, limit int) bool {
	return value != "" && len(value) <= limit && strings.TrimSpace(value) == value && strings.IndexFunc(value, unicode.IsControl) < 0
}

func validatePolicy(policy *apiv1.InteractiveAccessPolicy) error {
	if policy == nil {
		return nil
	}
	if policy.SourceWrites != nil && policy.SourceWrites.Mode != "pull-request" {
		return errors.New("interactive source writes require pull-request mode")
	}
	if err := validateHumans(policy.Humans.Viewers); err != nil {
		return err
	}
	if err := validateHumans(policy.Humans.Operators); err != nil {
		return err
	}
	seen := map[apiv1.InteractiveAction]bool{}
	for _, action := range policy.Actions {
		if !slices.Contains(actions, action) || seen[action] {
			return errors.New("interactive actions must be known and unique")
		}
		seen[action] = true
	}
	if len(policy.Credentials.Repositories) > 32 {
		return errors.New("interactive repository bindings exceed 32")
	}
	identities := map[apiv1.InteractiveRepositoryIdentity]bool{}
	for _, binding := range policy.Credentials.Repositories {
		if !validRepository(binding.Repository) || !validText(binding.CredentialRef, 128) || identities[binding.Repository] {
			return errors.New("interactive repository binding is invalid or duplicated")
		}
		identities[binding.Repository] = true
	}
	if policy.Credentials.Backlog != "" && !validText(policy.Credentials.Backlog, 128) {
		return errors.New("interactive backlog credential reference is invalid")
	}
	return nil
}

func validateHumans(grants []apiv1.InteractiveHumanGrant) error {
	if len(grants) > 128 {
		return errors.New("interactive human grants exceed 128")
	}
	for _, grant := range grants {
		if !validText(grant.Issuer, 2048) || strings.HasPrefix(grant.Issuer, "goobers/") || (grant.Subject == "") == (grant.Group == "") {
			return errors.New("interactive human requires an issuer and exactly one subject or group")
		}
		for _, value := range []string{grant.Subject, grant.Group} {
			if value != "" && !validText(value, 512) {
				return fmt.Errorf("interactive human subject or group is invalid")
			}
		}
	}
	return nil
}

func validRepository(id apiv1.InteractiveRepositoryIdentity) bool {
	if !validText(id.Owner, 256) || !validText(id.Name, 256) {
		return false
	}
	switch id.Provider {
	case apiv1.ProviderGitHub:
		return id.Project == ""
	case apiv1.ProviderADO:
		return validText(id.Project, 256)
	default:
		return false
	}
}
