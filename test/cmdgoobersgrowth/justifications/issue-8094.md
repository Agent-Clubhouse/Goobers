# Shared cloud claim composition (#8094)

The command adds dependency wiring for the shared claim resolver, durable repository registry, and epoch label-repair lifecycle. It reuses the existing control provider writer and its credential binding. Protocol transitions, policy verification, registry persistence, and reconciler lifecycle remain in internal packages; the command only supplies the controller pools, lease, journal, and pins.
