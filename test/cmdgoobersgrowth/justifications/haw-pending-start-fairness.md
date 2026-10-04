# Pending start sweep fairness

The daemon keeps a small in-memory cursor over immutable acceptance order and
wraps after each bounded pass. This prevents a full batch of capacity-held or
currently unsupported requests from permanently starving later eligible starts.
Custody, claims and query ordering remain in triggerqueue. Restarting the daemon
resets only sweep progress. No queue records are discarded and no baseline grows.
