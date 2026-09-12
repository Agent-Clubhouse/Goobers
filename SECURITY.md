# Security Policy

Goobers runs AI agents against real repositories with real credentials, so we take
security seriously. This file is the **repository disclosure policy**. For the product's
security & isolation model, see [`docs/requirements/security.md`](docs/requirements/security.md).

## Supported versions

Goobers is pre-1.0 and evolving quickly. Security fixes land on `main`; there are no
backported release branches yet. Always track the latest `main`.

## Verifying what you downloaded

Release tags are signed. The signing key is published as
[`.github/allowed_signers`](.github/allowed_signers). From a trusted current
checkout, set `TAG` to the release you downloaded and run:

```sh
TAG=v0.4.0-rc.2
git fetch origin "refs/tags/${TAG}:refs/tags/${TAG}"
git -c gpg.ssh.allowedSignersFile=.github/allowed_signers tag -v "${TAG}"
```

Published assets are covered by `SHA256SUMS`, which ships beside them:

```sh
sha256sum --check SHA256SUMS        # shasum -a 256 --check on macOS
```

The tag signature and checksum manifest answer different questions: the tag
authenticates the tag annotation and referenced source commit, while the
separately generated manifest detects changed asset bytes after download. The
tag does not authenticate `SHA256SUMS`; obtain it from the official GitHub
Release over authenticated HTTPS. `docs/guides/releases.md` describes these
boundaries.

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
