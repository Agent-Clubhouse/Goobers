# HAW-EVT-008: daemon provider read scope

The command package composes the existing scheduler, credential resolvers, and
provider factories. This slice threads its already captured generation through
backlog/refill/remediation counters and open-PR refreshers; it also wires the
shared ADO client at the existing stage factory. The small counterreadscope helper
sets explicit ordinary-automation launch policy and forms the refresher identity.
Reusable cache storage, partitioning, response replay and transport behavior stay
in internal/apireadcache. Trusted subprocess scope filtering stays in
internal/executor. Tests exercise those production seams without network services.
