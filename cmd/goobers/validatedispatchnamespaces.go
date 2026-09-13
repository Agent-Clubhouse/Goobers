package main

import (
	"context"
	"fmt"
	"io"

	"github.com/goobers/goobers/api/validate"
	"github.com/goobers/goobers/internal/instance"
)

// WarningDispatchNamespaceUnready (GNS001) fires when --check-dispatch-namespaces
// finds a gaggle's declared isolation.namespace that the CURRENT dispatch
// configuration cannot honor: the namespace does not exist, or this
// kubeconfig's credentials lack the RBAC grants the mode-3 dispatcher's
// create/cleanup path needs there (#4897 acceptance criterion 3). This is the
// exact check `goobers worker --dispatch-namespace` runs at startup — running
// it here, ahead of a rollout, is the upgrade/preflight diagnostic operators
// use to provision access before enabling or expanding mode-3 dispatch.
const WarningDispatchNamespaceUnready = "GNS001"

// checkGaggleDispatchNamespaces is advisory-only, exactly like
// checkRepositoryReality: a live cluster check never changes validate's exit
// code, because a config that is otherwise valid must stay valid to run in
// environments (a laptop, most CI) with no cluster credentials at all. When
// no usable kubeconfig/in-cluster config is found, it says so once and skips
// silently rather than failing — the same posture --check-repos takes when a
// provider token is absent.
func checkGaggleDispatchNamespaces(root, configDir string, set *instance.ConfigSet, stdout io.Writer, diagnostics *diagnosticCollector) {
	gaggleNamespaces, err := gaggleNamespacesFromConfig(set.Gaggles)
	if err != nil {
		pf(stdout, "dispatch namespaces not checked: %v\n", err)
		return
	}
	if len(gaggleNamespaces) == 0 {
		return
	}
	client, err := dispatchKubeClient()
	if err != nil {
		pf(stdout, "dispatch namespaces not checked: no usable Kubernetes credentials (%v); this is expected outside a cluster context\n", err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), dispatchNamespacePreflightTimeout)
	defer cancel()
	results, err := preflightGaggleNamespaces(ctx, client, gaggleNamespaces)
	if err != nil {
		pf(stdout, "dispatch namespace preflight incomplete: %v\n", err)
	}
	byNamespace := make(map[string]bool, len(results))
	for _, r := range results {
		byNamespace[r.Namespace] = r.Ready()
	}
	for gaggle, namespace := range gaggleNamespaces {
		if byNamespace[namespace] {
			continue
		}
		message := fmt.Sprintf("gaggle %q declares isolation.namespace %q, which the current dispatch configuration cannot honor "+
			"(does not exist, or this worker's credentials lack the RBAC grants dispatch needs there); provision it before enabling or "+
			"expanding mode-3 dispatch into this gaggle", gaggle, namespace)
		pln(stdout, "GNS001: "+message)
		diagnostics.add(diagnosticFile(root, configDir), "/gaggles/"+gaggle+"/spec/isolation/namespace",
			WarningDispatchNamespaceUnready, string(validate.Warning), message)
	}
}
