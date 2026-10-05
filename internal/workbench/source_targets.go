package workbench

import (
	"errors"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

func sourceBacklogIdentity(g apiv1.Gaggle) (apiv1.InteractiveRepositoryIdentity, error) {
	backlog := g.Spec.Backlog
	if backlog.BaseURL != "" {
		return apiv1.InteractiveRepositoryIdentity{}, errors.New("interactive backlog does not support custom provider endpoints")
	}
	switch backlog.Provider {
	case apiv1.ProviderGitHub:
		owner, name, ok := strings.Cut(backlog.Project, "/")
		if ok && targetSegment(owner) && targetSegment(name) {
			return apiv1.InteractiveRepositoryIdentity{Provider: backlog.Provider, Owner: owner, Name: name}, nil
		}
	case apiv1.ProviderADO:
		if g.Spec.Project.Provider == apiv1.ProviderADO && g.Spec.Project.BaseURL == "" && targetSegment(g.Spec.Project.Owner) && targetSegment(backlog.Project) {
			return apiv1.InteractiveRepositoryIdentity{Provider: backlog.Provider, Owner: g.Spec.Project.Owner, Project: backlog.Project}, nil
		}
	}
	return apiv1.InteractiveRepositoryIdentity{}, errors.New("backlog source has no unambiguous configured provider target")
}

func targetSegment(value string) bool {
	return textValue(value, 256) && strings.TrimSpace(value) == value && value != "." && value != ".." && !strings.ContainsAny(value, "/\\?#%:")
}

func validRepositoryIdentity(identity *apiv1.InteractiveRepositoryIdentity) bool {
	if identity == nil || !targetSegment(identity.Owner) || !targetSegment(identity.Name) {
		return false
	}
	switch identity.Provider {
	case apiv1.ProviderGitHub:
		return identity.Project == ""
	case apiv1.ProviderADO:
		return targetSegment(identity.Project)
	default:
		return false
	}
}
