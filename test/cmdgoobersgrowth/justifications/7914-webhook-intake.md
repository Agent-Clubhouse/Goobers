# #7914: cloud webhook intake

Growth: +62 non-test lines and +1 production file in `cmd/goobers-cloud`.

The combined control role mounts the webhook endpoint beside the API handler,
so the command package gains one composition file (`controlwebhook.go`: read
the secret file, build the API-pool store, wrap the handler) and a flag. The
intake itself, HMAC handling, recipient resolution, digest pinning and the
durable write all live in `internal/cloud/webhookintake`,
`internal/cloud/config` and `internal/cloud/acceptance`; the command package
only owns process options and the handler mux, which cannot move out of it.
