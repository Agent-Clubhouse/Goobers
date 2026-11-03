// Package durability provides atomic file writes, platform-specific replacement,
// and directory flush behavior for crash-safe state files. Reads and removals
// retry bounded transient Windows contention without hiding persistent errors.
package durability
