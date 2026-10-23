# Worker high-availability component

Include this component from the rainbow worker overlay after
`components/worker-base`. It applies the shared worker priority, 90-second
termination grace and zone spread to the `goobers` `WorkerDeployment`.

`worker-pdb-template.yaml` is a Temporal Worker Controller
`WorkerResourceTemplate`. The controller injects the exact build ID selector
and creates/deletes one PDB with each live worker version. Merge
`controller-values.yaml` into the Worker Controller chart values so its webhook
and service account may manage PDBs.
