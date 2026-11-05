# goobers decide: omit empty optional criteria

`goobers decide` sent empty optional criteria to the decision endpoint, which
rejected the request with a 422. This change drops empty criteria from the
request in the command's argument handling, plus a regression test.

The growth is limited to that fix in cmd/goobers/decide.go and its test. The
decision logic stays in internal/decisiongate and internal/decider, and no new
command, flag or result contract is added.
