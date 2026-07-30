package workflow

import apiv1 "github.com/goobers/goobers/api/v1alpha1"

func goobersForCapabilityAdmission(goobers map[string]apiv1.GooberSpec) map[string]apiv1.GooberSpec {
	if goobers == nil {
		return nil
	}
	filtered := make(map[string]apiv1.GooberSpec, len(goobers))
	for name, spec := range goobers {
		servers := make([]apiv1.MCPServer, len(spec.MCPServers))
		for i, server := range spec.MCPServers {
			servers[i] = server
			servers[i].CredentialRefs = nil
			for _, ref := range server.CredentialRefs {
				if ref.Kind != apiv1.MCPCredentialKindBYO {
					servers[i].CredentialRefs = append(servers[i].CredentialRefs, ref)
				}
			}
		}
		spec.MCPServers = servers
		filtered[name] = spec
	}
	return filtered
}
