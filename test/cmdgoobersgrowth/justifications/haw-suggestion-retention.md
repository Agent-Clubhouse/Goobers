# HAW-BACKLOG: preserve reviewed suggestion provenance

The daemon's existing journal and configuration retention paths need a small
adapter for operational suggestion review dependencies. The shared queue owns
bounded review state, actual origin pins and maintenance. This host-only glue
verifies the candidate journal's exact run/gaggle and joins its generation pins
with the existing session, restart and event pins. It adds no planning database.
