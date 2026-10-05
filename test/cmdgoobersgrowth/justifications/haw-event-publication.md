# HAW-EVT-006: host-local workflow event publication

This slice adds one command-package adapter for existing private runner factories,
configuration-generation archives, applied scheduler definitions and reload locks.
It binds publication to runner-owned journal scope and installs the typed executor
through the existing deterministic registry. The applied catalog swap holds the
publication fence through scheduler publication; unsaved configuration is never
permission or routing authority.

The protocol, outbox storage, retention inventory, provenance validation and
executor live in internal packages. No CLI command or duplicate scheduler was
added. Composed tests use the real archive builder, runner, queue and consumer.
