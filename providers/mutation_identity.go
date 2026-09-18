package providers

import (
	"net/url"
	"strings"
)

// MutationWorkItemURL returns an existing provider URL or derives the
// canonical GitHub work-item URL from a typed landing receipt.
func MutationWorkItemURL(provider, kind, id, existing string, confirmation *MergeConfirmation, admission *QueueAdmission, intent *LandingIntent) string {
	if existing != "" {
		return existing
	}
	repositoryAPIURL := mutationRepositoryAPIURL(confirmation, admission, intent)
	if !strings.EqualFold(provider, string(ProviderGitHub)) || repositoryAPIURL == "" {
		return ""
	}
	parsed, err := url.Parse(repositoryAPIURL)
	if err != nil {
		return ""
	}
	parts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	repos := -1
	for index, part := range parts {
		if part == "repos" && len(parts) > index+2 {
			repos = index
		}
	}
	if repos < 0 {
		return ""
	}
	switch {
	case strings.EqualFold(parsed.Hostname(), "api.github.com"):
		parsed.Scheme = "https"
		parsed.Host = "github.com"
		parsed.Path = ""
	case repos >= 2 && parts[repos-2] == "api" && parts[repos-1] == "v3":
		parsed.Path = "/" + strings.Join(parts[:repos-2], "/")
	default:
		return ""
	}
	parsed.RawPath, parsed.RawQuery, parsed.Fragment = "", "", ""
	segment := "issues"
	if kind == "pr" {
		segment = "pull"
	}
	parsed.Path = strings.TrimSuffix(parsed.Path, "/") + "/" + parts[repos+1] + "/" + parts[repos+2] + "/" + segment + "/" + id
	return parsed.String()
}

// MutationRepositoryAPIURL returns the repository address carried by the
// strongest available typed landing receipt.
func MutationRepositoryAPIURL(confirmation *MergeConfirmation, admission *QueueAdmission, intent *LandingIntent) string {
	return mutationRepositoryAPIURL(confirmation, admission, intent)
}

func mutationRepositoryAPIURL(confirmation *MergeConfirmation, admission *QueueAdmission, intent *LandingIntent) string {
	if confirmation != nil {
		return confirmation.RepositoryAPIURL
	}
	if admission != nil {
		return admission.RepositoryAPIURL
	}
	if intent != nil {
		return intent.RepositoryAPIURL
	}
	return ""
}
