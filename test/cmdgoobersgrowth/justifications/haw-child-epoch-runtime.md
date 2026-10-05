# HAW-CHD-008 contained execution epochs

The command adapter selects the exact accepted child's execution epoch for its
existing pod factory, blob custody, retained fork and credential lifetime. These
changes belong beside the daemon's existing private child dispatch references
and applied interactive-policy composition; no alternate queue, provider client,
or runner protocol is introduced.

Reusable result/fork custody and epoch bounds remain in childworkflow and
triggerqueue. The common human stage restart and generated runner retain their
existing preparation, retry, history and cancellation ownership. The runtime
holds one revocable interactive lease until nested actual worker owners join,
including stopped-worker import; per-request brokers independently check current
stage authority. Original accepted identity remains distinct from execution ID.

Public restart activation still requires the common daemon admission, durable
retry sweep and result observers to compose. This slice does not advertise a
complete restart UI and does not repin command growth or complexity baselines.
