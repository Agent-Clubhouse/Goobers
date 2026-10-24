# #8100 standalone Backprop attribution adapter

Eight non-test lines and no new Go files forward the optional run-attribution
read through the standalone dashboard's existing reader wrapper. Embedding the
narrower Reader interface otherwise hides this method from HTTP capability
detection and makes every standalone Backprop request return HTTP 501.

Only this composition adapter belongs in cmd/goobers. Attribution calculation,
bounded filesystem reads, scope validation, evidence integrity, and HTTP
authorization remain in their existing internal packages. The adapter neither
rewrites records nor changes workflow execution or recovery custody.
