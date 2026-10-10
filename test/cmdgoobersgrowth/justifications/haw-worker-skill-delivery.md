# HAW-CHD-009 captured skill delivery into pods

Three command-package lines connect existing owners: import the existing skill
resolver, select only the executing Goober's captured packages when composing
its kit, and pass the verified kit packages into the pod executor constructor.
These are adapters at the existing kit producer and consumer boundaries, not
new command-level policy or storage. Resolution and scope precedence remain in
internal/workflow; verified transport remains in internal/agentickit; safe
materialization and cleanup remain in internal/harness. No baseline changes or
new non-test command files are needed.
