package providers

import (
	"context"
	"encoding/base64"
	"reflect"
	"strings"
	"testing"
)

func TestGitAuthEnvironmentsPreserveHardenedShape(t *testing.T) {
	for _, name := range []string{
		"GIT_CONFIG_COUNT",
		"GIT_CONFIG_KEY_0",
		"GIT_CONFIG_VALUE_0",
		"GIT_CONFIG_KEY_9",
		"GIT_CONFIG_VALUE_9",
		"GIT_TERMINAL_PROMPT",
	} {
		t.Setenv(name, "inherited-"+name)
	}

	tests := []struct {
		name       string
		remoteURL  string
		build      func(*spyGitRegistrar) []string
		wantHeader string
		wantScrubs []string
	}{
		{
			name:      "GitHub",
			remoteURL: "https://github.com/acme/repo.git///",
			build: func(registrar *spyGitRegistrar) []string {
				return GitHubGitAuthEnvironment("github-token", "https://github.com/acme/repo.git///", registrar)
			},
			wantHeader: "basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:github-token")),
			wantScrubs: []string{"github-token", base64.StdEncoding.EncodeToString([]byte("x-access-token:github-token"))},
		},
		{
			name:      "Gitea",
			remoteURL: "https://gitea.example/acme/repo.git///",
			build: func(registrar *spyGitRegistrar) []string {
				return GiteaGitAuthEnvironment("gitea-token", "https://gitea.example/acme/repo.git///", registrar)
			},
			wantHeader: "basic " + base64.StdEncoding.EncodeToString([]byte("gitea-token:")),
			wantScrubs: []string{"gitea-token", base64.StdEncoding.EncodeToString([]byte("gitea-token:"))},
		},
		{
			name:      "ADO",
			remoteURL: "https://dev.azure.com/acme/project/_git/repo///",
			build: func(registrar *spyGitRegistrar) []string {
				env, err := ADOGitAuthEnvironment(
					context.Background(),
					NewADOPATCredentialSource("goobers", "ado-token"),
					registrar,
					"https://dev.azure.com/acme/project/_git/repo///",
				)
				if err != nil {
					t.Fatal(err)
				}
				return env
			},
			wantHeader: "Basic " + base64.StdEncoding.EncodeToString([]byte("goobers:ado-token")),
			wantScrubs: []string{
				"ado-token",
				base64.StdEncoding.EncodeToString([]byte("goobers:ado-token")),
				"Basic " + base64.StdEncoding.EncodeToString([]byte("goobers:ado-token")),
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			registrar := &spyGitRegistrar{}
			env := tc.build(registrar)
			wantSuffix := []string{
				"GIT_CONFIG_COUNT=2",
				"GIT_CONFIG_KEY_0=credential.helper",
				"GIT_CONFIG_VALUE_0=",
				"GIT_CONFIG_KEY_1=http." + strings.TrimRight(tc.remoteURL, "/") + "/.extraheader",
				"GIT_CONFIG_VALUE_1=AUTHORIZATION: " + tc.wantHeader,
				"GIT_TERMINAL_PROMPT=0",
			}
			if !reflect.DeepEqual(env[len(env)-len(wantSuffix):], wantSuffix) {
				t.Fatalf("environment suffix = %#v, want %#v", env[len(env)-len(wantSuffix):], wantSuffix)
			}
			for _, entry := range env[:len(env)-len(wantSuffix)] {
				name, _, _ := strings.Cut(entry, "=")
				upper := strings.ToUpper(name)
				if upper == "GIT_CONFIG_COUNT" || upper == "GIT_TERMINAL_PROMPT" ||
					strings.HasPrefix(upper, "GIT_CONFIG_KEY_") || strings.HasPrefix(upper, "GIT_CONFIG_VALUE_") {
					t.Errorf("inherited Git configuration survived: %q", entry)
				}
			}
			gotScrubs := make([]string, len(registrar.secrets))
			for i, secret := range registrar.secrets {
				gotScrubs[i] = string(secret)
			}
			if !reflect.DeepEqual(gotScrubs, tc.wantScrubs) {
				t.Errorf("registered scrub forms = %#v, want %#v", gotScrubs, tc.wantScrubs)
			}
		})
	}
}

func TestGitAuthEnvironmentsWithoutTokenRemainHardened(t *testing.T) {
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "credential.helper")
	t.Setenv("GIT_CONFIG_VALUE_0", "unsafe-helper")
	t.Setenv("GIT_TERMINAL_PROMPT", "1")

	for _, tc := range []struct {
		name  string
		build func(*spyGitRegistrar) []string
	}{
		{
			name: "GitHub",
			build: func(registrar *spyGitRegistrar) []string {
				return GitHubGitAuthEnvironment("", "https://github.com/acme/public.git", registrar)
			},
		},
		{
			name: "Gitea",
			build: func(registrar *spyGitRegistrar) []string {
				return GiteaGitAuthEnvironment(" ", "https://gitea.example/acme/public.git", registrar)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			registrar := &spyGitRegistrar{}
			env := tc.build(registrar)
			if got := env[len(env)-1]; got != "GIT_TERMINAL_PROMPT=0" {
				t.Fatalf("last environment entry = %q, want GIT_TERMINAL_PROMPT=0", got)
			}
			for _, entry := range env[:len(env)-1] {
				upper := strings.ToUpper(entry)
				if strings.HasPrefix(upper, "GIT_CONFIG_") || strings.HasPrefix(upper, "GIT_TERMINAL_PROMPT=") {
					t.Errorf("anonymous environment retained Git configuration: %q", entry)
				}
			}
			if len(registrar.secrets) != 0 {
				t.Errorf("anonymous environment registered scrub forms: %#v", registrar.secrets)
			}
		})
	}
}
