package providers

import (
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

func TestCloneURL(t *testing.T) {
	tests := []struct {
		name    string
		ref     apiv1.RepoRef
		want    string
		wantErr string
	}{
		{
			name: "github",
			ref:  apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web"},
			want: "https://github.com/acme/web.git",
		},
		{
			name: "azure devops",
			ref:  apiv1.RepoRef{Provider: apiv1.ProviderADO, Owner: "acme/widgets", Name: "web"},
			want: "https://dev.azure.com/acme/widgets/_git/web",
		},
		{
			name: "azure devops explicit project",
			ref:  apiv1.RepoRef{Provider: apiv1.ProviderADO, Owner: "acme", Project: "widgets", Name: "web"},
			want: "https://dev.azure.com/acme/widgets/_git/web",
		},
		{
			name: "azure devops escapes path segments",
			ref:  apiv1.RepoRef{Provider: apiv1.ProviderADO, Owner: "acme/widgets project", Name: "web app"},
			want: "https://dev.azure.com/acme/widgets%20project/_git/web%20app",
		},
		{
			name:    "unknown provider",
			ref:     apiv1.RepoRef{Provider: "unknown", Owner: "acme", Name: "web"},
			wantErr: `runner: unsupported repo provider "unknown"`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := CloneURL(tt.ref)
			if tt.wantErr != "" {
				if err == nil || err.Error() != tt.wantErr {
					t.Fatalf("CloneURL() error = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("CloneURL() error = %v", err)
			}
			if got != tt.want {
				t.Fatalf("CloneURL() = %q, want %q", got, tt.want)
			}
		})
	}
}
