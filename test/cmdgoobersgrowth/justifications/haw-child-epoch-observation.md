# Current child epoch lifecycle

Daemon orchestration follows the active execution while retaining the original
start receipt. Its existing bounded queue drain retries an accepted epoch and
records exact cancellation/result observations; stale execution observations
cannot overwrite the current epoch. Generation routing remains daemon-specific.
The new callback shares common restart admission rather than introducing a
second resume system. Queue custody and transition validation remain reusable.
No command-growth baseline changes are included.
