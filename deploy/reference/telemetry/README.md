# Customer-owned Application Insights overlay

This overlay adds direct Azure Monitor export to the reference API deployment.
It keeps the connection string in a Kubernetes Secret and writes only the
environment-variable reference plus the `standard` collection profile to
`instance.yaml`. The existing journal PVC also persists the bounded telemetry
replay spool across pod replacement.

Create the Secret without putting its value in shell history or a checked-in
manifest:

```sh
kubectl --namespace goobers-system create secret generic goobers-application-insights \
  --from-file=connection-string=/secure/application-insights.txt \
  --dry-run=client -o yaml | kubectl apply -f -
```

`/secure/application-insights.txt` contains only the connection string;
`kubectl` stores its value under the `connection-string` key.
`application-insights-secret.example.yaml` documents
the equivalent Secret shape but must not be populated and committed.

Render and apply this directory as the overlay, after making the same image,
TLS, authentication, storage, and replica changes required by the base:

```sh
kubectl kustomize deploy/reference/telemetry
kubectl apply -k deploy/reference/telemetry
```

The init container runs the same `goobers telemetry configure` command used on
Windows, macOS, and Linux. It does not resolve or emit the Secret. The API
container resolves the reference at startup and asynchronous delivery never
blocks workflow work.

After the API pod is running, use the shared connectivity conformance test:

```sh
kubectl --namespace goobers-system exec deploy/goobers-api -- \
  goobers telemetry test /var/lib/goobers
```

This fixed, identity-free probe bypasses replay. A failed probe exits nonzero
but does not change the running daemon or stop workflow execution. The base's
default-deny egress policy deliberately does not grant arbitrary telemetry
egress; authorize the configured Application Insights ingestion endpoint with
the tenant's CNI, egress gateway, or HTTPS proxy policy.
