# HAW-CHD-003 signed child custody identity

The daemon blob adapter now requires the authenticated signed child marker and
refuses missing custody even when both queue and journal records are absent.
This is a small extension to the command's existing queue/journal scope bridge.
Token minting and verification live in internal/podauth; the HTTP principal only
carries the verified marker. A distinct MAC domain prevents token relabeling.
