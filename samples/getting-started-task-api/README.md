# Getting Started task API

This deliberately imperfect TypeScript service is the disposable target for
Goobers onboarding. It is small enough for a first autonomous change: Node's
built-in HTTP server, in-memory data, and two development dependencies.

Use it only through the disposable GitHub-backed stage of the
[canonical quickstart](../../docs/guides/quickstart.md#2-graduate-to-the-token-bearing-quickstart-template).
That guide owns the ordered setup and run commands; this page documents the
fixture itself.

## Version contract

[`sample.json`](sample.json) is the machine-readable contract. Its `version`
identifies the app and its matching [`seed-issues.json`](seed-issues.json) as
one immutable tutorial fixture, and `compatibleTemplates` pins the shipped
onboarding workflow contract it exercises. Onboarding consumers must select an
exact sample version from an exact Goobers release tag or commit rather than
copying the current default branch. The stable hosted-repository mapping can
change without changing this fixture contract.

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

## Workflow coverage

```text
go test ./cmd/goobers -run '^TestGettingStartedSampleQuickstartThroughRealRunner$'
go test ./cmd/goobers -run '^TestGettingStartedSampleImplementationLocalCIThroughRealRunner$'
```

Run these acceptance checks from the Goobers source repository. The quickstart
check initializes the actual embedded `quickstart@v1` template, materializes the
pinned fixture in a temporary Git repository, and drives the real Goobers local
runner through backlog claim, implementation, review, branch push, and the
production `open-pr` command.

The implementation check uses the same seed issue and repository with the
flagship implementation workflow's `implement` -> reviewer gate -> `local-ci`
shape. It proves that the gaggle CI override is compiled through
`ApplyGaggleCICommand` and executed by the real shell executor as
`npm run ci`, asserting that a valid implementation passes the `local-ci` stage
and a deliberately broken TypeScript implementation fails it.
Only the external coding-model adapter and GitHub HTTP endpoint are replaced
with deterministic test doubles.

## Disposal

The app binds only to loopback, keeps all task state in process memory, and
uses no database, cloud service, credentials, or adopter-owned repository.
Installation and compilation write only `node_modules/` and `dist/` inside the
throwaway checkout. Stop the process, delete any throwaway remote repository
or fork created for the PR exercise, then delete the checkout; no application
state remains elsewhere.
