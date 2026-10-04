package eventing

import (
	"encoding/json"
	"errors"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

// ValidateIngress bounds exact authenticated producer namespaces. Internal
// worker, child and session issuers can never obtain an external binding.
func ValidateIngress(policy *apiv1.GaggleEvents) error {
	if policy == nil {
		return nil
	}
	if len(policy.Ingress) > 128 {
		return errors.New("eventing: too many ingress bindings")
	}
	seen := map[string]bool{}
	for _, b := range policy.Ingress {
		if !consumerName.MatchString(b.Name) || seen[b.Name] || !boundedText(b.Issuer, 2048) || strings.HasPrefix(b.Issuer, "goobers/") || !boundedText(b.Subject, 512) || len(b.AllowedTypes) == 0 || len(b.AllowedTypes) > 32 {
			return errors.New("eventing: invalid or repeated ingress binding")
		}
		seen[b.Name] = true
		types := map[string]bool{}
		for _, kind := range b.AllowedTypes {
			if !boundedText(kind, 256) || types[kind] {
				return errors.New("eventing: invalid ingress type ceiling")
			}
			types[kind] = true
			raw, _ := json.Marshal(map[string]string{"specversion": "1.0", "id": "validation", "source": b.Source, "type": kind})
			if _, err := Parse(raw); err != nil {
				return err
			}
		}
	}
	return nil
}
