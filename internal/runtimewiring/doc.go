// Package runtimewiring assembles executors, credentials, harness preflight,
// configuration snapshots, and recovery safeguards shared by the local daemon
// and cloud worker. Binary entry points supply their storage and control-plane
// composition without adding cloud dependencies to the local runtime.
package runtimewiring
