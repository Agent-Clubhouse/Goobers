# Child human-admission review

Out-of-distribution child proposals can be held for human admission review
when the optional novelty approval policy is enabled. The durable hold, its
dispatch fence and decision recording live in internal/triggerqueue; admission
evidence and the operator CLI live in internal/childworkflow. The command layer
only registers `goobers workflow child-admission`, opens the existing scheduler
queue without creating one, refuses dispatch of a held child until it is
approved, journals the approving decision in the pinned child lineage and gives
human rejections a distinct terminal summary. Read-only portal history adds a
single adapter over the existing queue handle.
