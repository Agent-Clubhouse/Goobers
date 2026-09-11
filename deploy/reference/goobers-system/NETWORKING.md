# Control-plane network floor

The base applies default-deny ingress and egress to **every** pod in
`goobers-system`, including Linux and Windows workers, the disabled daemon/API
and operator deployments, and operator-supplied proxy/collector pods. A CNI that
enforces NetworkPolicy on every participating OS is required; labels alone do
not enforce isolation. No policy is applied to a live cluster by repository CI.

Allowed paths in the base:

| Source | Destination | Port |
|---|---|---|
| All namespace pods | kube-system pods labeled `k8s-app: kube-dns` | UDP/TCP 53 |
| Workers | goobers-temporal namespace | TCP 7233 |
| Daemon (`component: api`) | Temporal pods in goobers-temporal | TCP 7233 |
| Daemon and workers | same-namespace `name: goobers-egress-proxy` | TCP 3128 |
| Forward proxy | public IPv4, with private/link-local/loopback exclusions | TCP 443 |
| Daemon | same-namespace `name: goobers-collector` | TCP 4317 |
| Workers | Daemon (`component: api`) | TCP 8080 |
| Stage pods in any gaggle namespace (`goobers.dev/gaggle-namespace: "true"`) | Daemon (`component: api`) | TCP 8080 |

`name` and `component` above mean the `app.kubernetes.io/` label keys. Proxy and
collector ingress rules allow only the named clients in the same namespace.
The proxy's DNS permission does not give it access to private HTTPS targets;
RFC1918 and all `169.254.0.0/16` addresses, including IMDS, remain excluded.
There is no public IPv6 grant. NetworkPolicy grants compose additively: an
overlay-wide Internet grant can undo these restrictions and must be reviewed.

TCP 8080 is the daemon's single API **and** blob-plane port
(`internal/netpolrender.DefaultBlobEndpoint().Port`) — every dispatched
stage's journal-emit, blob-put, artifact-record and surrender call targets
it, alongside ordinary API traffic. The stage-pod grant selects gaggle
namespaces generically via the `goobers.dev/gaggle-namespace: "true"` label
(`../gaggle-namespace/base/namespace.yaml`); a gaggle namespace that omits
this label is invisible to the grant and its stage pods cannot reach the
daemon at all (#4828). See #3585 for narrowing this same grant further, to
specific stage-pod runner classes, once the operator wants defense-in-depth
beyond "any stage pod, any gaggle."

## Required adopter configuration

This reference does **not** install a forward proxy or collector. Supply a proxy
with the label/port above, configure its hostname allowlist and internal CA,
and configure daemon/worker process proxy settings. Keep proxy bypasses limited
to the internal services actually granted. A hostname allowlist remains
essential: TCP443 to a public IP is not a hostname policy. Do not grant direct
Internet egress to workers to bypass a missing proxy. Collector export egress
must be separately authorized for the chosen backend.

Before enabling the daemon or operator, add cluster-specific overlay policies
for Kubernetes API access and user/ingress-controller access to the API. Use
the actual API endpoint CIDR/port and namespace-plus-pod ingress selectors;
there is no portable API-server CIDR and this base deliberately invents none.
For private endpoints, add narrow destination grants only to the components
that require them, never to the public proxy. Add the cluster's Pod, Service,
node and API CIDRs to the proxy
exclusions too, particularly if those networks use publicly routed addresses.
Validate the effective policy
union for the overlay with positive connection probes and negative private/
IMDS probes on both Linux and Windows before rollout. An unchanged reference
without these adopter settings fails closed; it is not a ready-to-apply cluster.

This floor fixes the namespace-policy mismatch, not multi-tenant isolation.
Shared workers still share filesystem, service identity and other process
resources across same-operator gaggles. Stage pods retain their separate
per-runner-class policies in gaggle namespaces.
