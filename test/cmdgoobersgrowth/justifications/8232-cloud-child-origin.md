# Cloud child-origin host wiring (#8232)

This PR adds two production lines to cmd/goobers-cloud and no production files:
one assigns the existing resident worker identity to the child-origin HTTP seam;
the other connects control's Temporal client and fenced journal writer to its
child authority service. They belong at the existing worker/control composition
points. Activity-loan verification, HTTP transport and authorization, and origin
construction remain in internal packages. There is no growth in other commands.
