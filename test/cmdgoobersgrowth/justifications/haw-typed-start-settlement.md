# Connect typed queued-start disposition to the child result observer

The existing daemon child terminal observer now reads the durable start control
when capturing a rejected, unstarted child. It maps the verified cancellation to
the existing cancelled terminal result and retains the actual disposal timestamp.
This small composition belongs beside the journal/workspace observer; atomic
source settlement and all session turn handling remain in internal/triggerqueue.
