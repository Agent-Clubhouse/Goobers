# #8213: bind child terminal writes through parent pins

The command package adds three non-test lines to pass the acceptance store's
durable child-custody reader into the shared control provider writer.

All child-parent resolution, pin inheritance, journal agreement checks and
permanent refusals remain in `internal/cloud/terminalwriter`.
