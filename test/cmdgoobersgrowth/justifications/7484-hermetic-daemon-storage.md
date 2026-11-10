# #7484: hermetic daemon storage admission

The four added non-test lines introduce a constructor seam in `cmd/goobers`
so in-process daemon tests can disable host-disk admission in `TestMain`.
The production binding still calls the same storage-gate constructor with
the same resolved configuration and defaults. No non-test file is added.

The seam belongs beside the daemon's existing construction code: it selects
the dependency used by command-owned daemon fixtures, while reusable storage
policy remains in `internal/localscheduler` and `internal/instance`. Moving
this test-construction choice into those packages would expose a command-test
concern to production library callers. The fixture and default-preservation
assertions live in test files and do not count toward this growth.
