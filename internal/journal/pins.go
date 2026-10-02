package journal

// PinnedRun returns the immutable identity fields held by the writer, without
// rereading files that an unconfined same-UID stage could have changed.
func (r *Run) PinnedRun() (runID, workflowDigest, gooberDigest string) {
	return r.id.RunID, r.id.WorkflowDigest, r.id.GooberDigest
}

// Branch is the writer's branch scope. Branch changes are controlled by the
// runner; parallel execution uses independent branch-scoped journal wrappers.
func (r *Run) Branch() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.branch
}
