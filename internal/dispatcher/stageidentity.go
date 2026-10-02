package dispatcher

import apiv1 "github.com/goobers/goobers/api/v1alpha1"

func (c Config) serviceAccountFor(gaggle string) string {
	return (apiv1.GaggleIsolation{ServiceAccount: c.GaggleServiceAccounts[gaggle]}).EffectiveServiceAccount()
}

// ServiceAccounts resolves the same account map for rendering and namespace preflight.
func ServiceAccounts(gaggles []apiv1.Gaggle) map[string]string {
	accounts := make(map[string]string, len(gaggles))
	for _, g := range gaggles {
		accounts[g.Name] = g.Spec.Isolation.EffectiveServiceAccount()
	}
	return accounts
}
