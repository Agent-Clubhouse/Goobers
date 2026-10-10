# Parallel parent execution ownership (LAND-C06)

The daemon composes retained generations, exact child/parent signed attempts,
shared journal writers and host-owned repository recovery. These additions wire
branch-specific physical custody and fork archive/retirement into those existing
owners. Source capture and fork/result joins live in internal/parallelworkspace;
Git restoration stays in internal/recovery and internal/worktree; scheduling and
capacity arbitration remain in internal/runner. No alternate scheduler or private
authentication system is added. Public admission remains refused until qualified.
