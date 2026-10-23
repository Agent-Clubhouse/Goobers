# Worker high-availability component

Include this component from the rainbow worker overlay after
`components/worker-base`. It applies the shared worker priority, staged 360-second
termination grace and zone spread to the `goobers` `WorkerDeployment`.

Do not lower the grace to 90 seconds until `workerhost.ChildSettlementTimeout`
is lowered in the same change and CHAOS-13 qualifies rainbow re-attachment.

`worker-pdb-template.yaml` is a Temporal Worker Controller
`WorkerResourceTemplate`. The controller injects the exact build ID selector
and creates/deletes one PDB with each live worker version. Merge
`controller-values.yaml` into the Worker Controller chart values so its webhook
and service account may manage PDBs. Pin Worker Controller v1.13.0 (Helm chart
0.32.0), whose selector injection adds `temporal.io/build-id` and
`temporal.io/deployment-name`. The template also carries non-empty
`matchExpressions`, so an uninjected selector never selects unrelated pods.

#7888's rainbow overlay delete-patches the legacy broad PDB before including
this component; the legacy PDB remains only in the non-rainbow render.
PriorityClasses are prerequisites owned by #7852's cloud reference; this
component references `goobers-control-plane` and never redefines it.
