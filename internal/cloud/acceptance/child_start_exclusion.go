package acceptance

// childLineageOwned matches an acceptance row that belongs to a generated
// child. Such a child is started only by its Temporal parent (the engine-owned
// start intent in control.child_engine_start, bound after AcceptChild), never
// by the ordinary start loop's Pending or Uncertain scans. RetainedPage still
// sees it: a frozen child keeps its pinned configuration generation alive.
const childLineageOwned = `SELECT 1 FROM control.child_lineage AS c
		WHERE c.instance_id=a.instance_id AND c.acceptance_id=a.id`
