# Fix main: undefined crossProviderBacklog after concurrent merges

Growth: +1 net non-test line in `cmd/goobers`: the `providerconfig` import in
`fleethealth_claimability.go`.

3163270c2 removed the `crossProviderBacklog` pass-through wrapper from
`adoprovider.go`, because it only forwarded to `providerconfig.CrossProviderBacklog`.
#7027 (0dabb533b) was written against the earlier base and still called the wrapper.
Each PR passed on its own base; together they broke `main`
(`vet: cmd/goobers/fleethealth_claimability.go:55:5: undefined: crossProviderBacklog`).
The fix calls `providerconfig.CrossProviderBacklog` directly, matching the intent of
3163270c2. The line is the import that call needs, so it can't live elsewhere.
