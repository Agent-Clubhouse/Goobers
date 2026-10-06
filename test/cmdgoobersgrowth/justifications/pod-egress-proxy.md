# feat/pod-egress-proxy

`cmd/goobers/workerdispatch.go` grows by one line: the worker hands
`runner.podEgressProxy` from the loaded instance config (which a stage pod
cannot read) to `dispatcher.Config.PodEgressProxy`, beside the other
`dispatcher.Config` fields. Validation lives in `internal/instance` and the
pod-spec stamping in `internal/dispatcher`.
