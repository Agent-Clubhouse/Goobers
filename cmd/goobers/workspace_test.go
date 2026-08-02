package main

import (
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
)

func TestResolvePinnedWorkspaceProject(t *testing.T) {
	set := &instance.ConfigSet{Gaggles: []apiv1.Gaggle{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "web"},
			Spec: apiv1.GaggleSpec{Project: apiv1.RepoRef{
				Provider: apiv1.ProviderGitHub,
				Owner:    "acme",
				Name:     "web",
				Checkout: &apiv1.CheckoutSpec{Mode: apiv1.CheckoutModePinned},
			}},
		},
	}}
	for _, selector := range []string{"web", "acme/web", "ACME/WEB"} {
		got, err := resolvePinnedWorkspaceProject(set, selector)
		if err != nil {
			t.Fatalf("selector %q: %v", selector, err)
		}
		if got.Name != "web" {
			t.Fatalf("selector %q resolved %q", selector, got.Name)
		}
	}
}

func TestResolvePinnedWorkspaceProjectRejectsDisposableRepo(t *testing.T) {
	set := &instance.ConfigSet{Gaggles: []apiv1.Gaggle{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "web"},
			Spec: apiv1.GaggleSpec{Project: apiv1.RepoRef{
				Provider: apiv1.ProviderGitHub,
				Owner:    "acme",
				Name:     "web",
			}},
		},
	}}
	if _, err := resolvePinnedWorkspaceProject(set, "web"); err == nil ||
		!strings.Contains(err.Error(), "not configured for pinned checkout") {
		t.Fatalf("error = %v", err)
	}
}
