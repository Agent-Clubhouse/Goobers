# #8085 (d): blob plane canary between control and workers

`cmd/goobers-cloud` grows by the composition of a same-store check:
- `putBlobPlaneCanary`: control puts the instance's canary blob when it composes the engine child protocol.
- `probedChildAuthority`: the worker wraps its engine child authority so that authority is withheld until that canary is readable through its stage blob plane (#8188).

The logic lives below `cmd`:
- the canary's identity is `childresult.BlobPlaneCanary`;
- the paced, sticky probe is `childadmissionclient.ProbedAuthority`.

The command layer only connects them to the role's object store, the worker's blob store, and its log. It adds no command and no new authority path.
