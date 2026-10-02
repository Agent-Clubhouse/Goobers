# #2230: native harness skill packages

The CLI wiring adds 19 non-test lines and no files to pass captured skill packages into local and worker harness executors and enforce pinned configuration snapshots. Package capture, safe installation, and cleanup live in internal/harness and internal/gitexclude; the command changes connect those library boundaries to existing execution paths.
