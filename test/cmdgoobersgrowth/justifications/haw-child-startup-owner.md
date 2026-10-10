# LAND-C06 generated-child startup ownership

Ten lines in the existing daemon startup composition defer a nonterminal
accepted child to its already-wired durable queue after validating its pinned
generation. The queue owns physical custody reconciliation and exclusive
journal handoff. This decision belongs in the loop that currently chooses
between local resume and external ownership; it adds no new execution,
storage, token or workspace logic to cmd/goobers.
