// Package workspaceauthority validates journal-pinned workspace custody for
// cleanup and recovery. The local runner and remote worker read the same
// receipts here. These readers do not create a runner, issue custody, restore
// a checkout, or replace the live ownership and archive-custody checks performed
// by the caller. Missing or inconsistent evidence remains a refusal.
package workspaceauthority
