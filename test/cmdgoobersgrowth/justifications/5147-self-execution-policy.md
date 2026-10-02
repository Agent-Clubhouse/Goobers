# #5147: explicit self-execution policy

The command layer adds 112 non-test lines and one file to connect the typed placement policy to daemon, worker, admission, and status boundaries. Policy parsing and enforcement live in internal packages. The command additions are required to apply the same policy before local side effects, including the first worker snapshot, and report actual local execution observations.
