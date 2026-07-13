# Security Policy

Goobers runs AI agents against real repositories with real credentials, so we take
security seriously. This file is the **repository disclosure policy**. For the product's
security & isolation model, see [`docs/requirements/security.md`](docs/requirements/security.md).

## Supported versions

Goobers is pre-1.0 and evolving quickly. Security fixes land on `main`; there are no
backported release branches yet. Always track the latest `main`.

## Reporting a vulnerability

**Please do not report security vulnerabilities through public GitHub issues, pull
requests, or discussions.**

Instead, report privately through GitHub's built-in flow:

1. Go to the repository's **Security** tab.
2. Click **Report a vulnerability** (Privately report a vulnerability).
3. Provide as much detail as you can (see below).

This opens a private security advisory visible only to you and the maintainers.

Please include:

- A description of the issue and its impact.
- Steps to reproduce (proof-of-concept if possible).
- Affected component/path and, if known, the commit or version.
- Any suggested remediation.

## What to expect

- **Acknowledgement** within a few business days.
- An assessment and, where valid, a fix tracked in a private advisory until a patch is ready.
- **Coordinated disclosure**: we'll agree on a disclosure timeline with you and credit you
  in the advisory unless you prefer to remain anonymous.

Please act in good faith: give us reasonable time to remediate before any public
disclosure, and avoid privacy violations, data destruction, or service disruption while
testing.
