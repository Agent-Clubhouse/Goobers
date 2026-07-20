# GitHub webhook triggers

A workflow can wake immediately on GitHub events while keeping its existing
gather stage as the source of authoritative state:

```yaml
spec:
  triggers:
    - type: webhook
      events: [pull_request, issues, check_suite]
```

Configure one shared secret for the instance in `instance.yaml`. The value must
come from an environment variable or an owner-only file; never put it inline.

```yaml
webhook:
  listen: 127.0.0.1:8081 # optional; this is the default
  secret:
    env: GITHUB_WEBHOOK_SECRET
```

Set the GitHub webhook payload URL to `/webhooks/github`, content type to JSON,
and secret to the same value. The daemon verifies `X-Hub-Signature-256` before
mapping `X-GitHub-Event` to subscribed workflows. The payload is not passed to
the run; each delivery is only a wake-up edge, and normal readiness limits and
run budgets still apply. Duplicate `X-GitHub-Delivery` IDs are acknowledged
without firing twice during one daemon lifetime.

The webhook listener is not opened unless both a webhook trigger and
`webhook.secret` are configured. It accepts only loopback addresses. Exposing
that listener and applying any public-edge access policy is an operator task,
for example:

```sh
# Tailscale Funnel
tailscale funnel --bg 8081

# Cloudflare quick tunnel
cloudflared tunnel --url http://127.0.0.1:8081

# SSH remote forwarding (the remote host controls public exposure)
ssh -R 8081:127.0.0.1:8081 webhook-edge.example.com
```

Configure the edge to forward only the GitHub webhook endpoint and protect the
secret as you would any other instance credential. Restrict the public edge to
GitHub's published webhook source ranges and rate-limit rejected requests.

With `goobers up --watch-config`, edits to event lists and additional webhook
triggers reload normally while the listener is active. Adding the first or
removing the last webhook trigger changes whether a socket exists and is
rejected as a live reload; restart the daemon to apply that topology change.
