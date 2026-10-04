package eventing

import (
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

func TestIngressConfigurationRefusesAmbiguousAuthority(t *testing.T) {
	good := &apiv1.GaggleEvents{Ingress: []apiv1.EventIngressBinding{{Name: "builds", Issuer: "https://identity.example", Subject: "producer", Source: "urn:builds", AllowedTypes: []string{"build.finished"}}}}
	if err := ValidateConfiguration(good); err != nil {
		t.Fatal(err)
	}
	for _, edit := range []func(*apiv1.GaggleEvents){
		func(p *apiv1.GaggleEvents) { p.Ingress = append(p.Ingress, p.Ingress[0]) },
		func(p *apiv1.GaggleEvents) { p.Ingress[0].Name = "../other" },
		func(p *apiv1.GaggleEvents) { p.Ingress[0].Issuer = "goobers/session" },
		func(p *apiv1.GaggleEvents) { p.Ingress[0].Issuer = "" },
		func(p *apiv1.GaggleEvents) { p.Ingress[0].Subject = "*\n" },
		func(p *apiv1.GaggleEvents) { p.Ingress[0].Source = "bad source" },
		func(p *apiv1.GaggleEvents) { p.Ingress[0].AllowedTypes = nil },
		func(p *apiv1.GaggleEvents) { p.Ingress[0].AllowedTypes = []string{"x", "x"} },
		func(p *apiv1.GaggleEvents) { p.Ingress[0].AllowedTypes = []string{strings.Repeat("x", 257)} },
	} {
		p := good.DeepCopy()
		edit(p)
		if err := ValidateConfiguration(p); err == nil {
			t.Fatal("invalid ingress allowed", p)
		}
	}
	copied := good.DeepCopy()
	copied.Ingress[0].AllowedTypes[0] = "other"
	if good.Ingress[0].AllowedTypes[0] != "build.finished" {
		t.Fatal("ingress copy aliases authority")
	}
}
