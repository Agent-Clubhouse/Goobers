# Getting Started task API

This deliberately imperfect TypeScript service is the disposable target for
Goobers onboarding. It is small enough for a first autonomous change: Node's
built-in HTTP server, in-memory data, and two development dependencies.

## Version contract

[`sample.json`](sample.json) is the machine-readable contract. Its `version`
identifies the app and its matching [`seed-issues.json`](seed-issues.json) as
one immutable tutorial fixture. Onboarding consumers must select an exact
sample version from an exact Goobers release tag or commit rather than copying
the current default branch. The stable hosted-repository mapping can change
without changing this fixture contract.

When materialized, the contents of this directory become the root of a new
throwaway repository, including `.github/workflows/ci.yml`. Do not run the
tutorial against the Goobers source checkout itself.

## Run locally

Node.js 20 or newer is required.

```text
npm run ci
npm start
```

The service listens on `127.0.0.1:3000` by default. Set `PORT` to choose
another port.

```text
GET  /health
GET  /tasks
POST /tasks
PATCH /tasks/:id/complete
```

## Seed the backlog

`seed-issues.json` contains the labels and complete issue bodies needed by the
getting-started flow. A hosted flow can create those labels and issues in the
throwaway repository. An offline flow can read and display the same catalog
without contacting GitHub. The issue order is stable; the first entry is the
shortest happy-path implementation.

## Quickstart compatibility

```text
go test ./cmd/goobers -run '^TestGettingStartedSampleQuickstartThroughRealRunner$'
```

Run this acceptance check from the Goobers source repository. It materializes
the pinned fixture in a temporary Git repository and drives the real Goobers
local runner through backlog claim, implementation, review, branch push, and
the production `open-pr` command. Only the external coding-model adapter and
GitHub HTTP endpoint are replaced with deterministic test doubles. The check
asserts that the pushed branch resolves the first seeded issue and that the
provider receives the resulting pull request.

## Disposal

The app binds only to loopback, keeps all task state in process memory, and
uses no database, cloud service, credentials, or adopter-owned repository.
Installation and compilation write only `node_modules/` and `dist/` inside the
throwaway checkout. Stop the process, delete any throwaway remote repository
or fork created for the PR exercise, then delete the checkout; no application
state remains elsewhere.
