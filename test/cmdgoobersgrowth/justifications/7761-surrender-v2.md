# #7761: Stamp the trusted-usage surrender contract

Growth: +1 non-test line and +0 production files in `cmd/goobers`.

The in-pod dispatch supervisor now stamps the current version on the
`SurrenderedResult` it already constructs. The version contract and
compatibility validation live in `internal/dispatcher`; this single
command-package field assignment is the required wire boundary.
