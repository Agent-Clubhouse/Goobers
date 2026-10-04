# Interactive human access

Interactive access is opt-in per gaggle. Existing monitoring retains its instance
role behavior. New interactive sessions, provider-backed browsing and writes
require both a verified instance role and an explicit gaggle grant. An instance
administrator has no implicit gaggle grant. Anonymous loopback requests cannot
use the interactive permission route.

The current implementation provides policy authorization, named credential
selection and `GET /api/v1/gaggles/{gaggle}/interactive-capabilities`. It does not
implement sessions, source editing or new intervention operations yet. The
response separates `authorized`, `credentialConfigured` and `available`, and
states a disabled reason for each action. A configured grant is not a claim that
an operation is implemented.

## Human membership

```yaml
# In instance.yaml api.auth.oidc, alongside issuer/audience/roles:
groupsClaim: groups
```

The group claim defaults to `groups` and is independent of instance role mapping.
Only a verified JWT supplies group membership. Missing, malformed or oversized
group claims grant no groups, while preserving authenticated subject and
instance roles for existing monitoring. Grant identities use exact issuer and
subject or group, never display names.

```yaml
# Gaggle spec:
interactiveAccess:
  humans:
    viewers:
      - issuer: https://identity.example
        group: factory-readers
    operators:
      - issuer: https://identity.example
        subject: alice
  actions: [backlog.read, backlog.edit, repository.read, source.proposeChange]
  credentials:
    backlog: human-issues
    repositories:
      - repository: {provider: github, owner: acme, name: web}
        credentialRef: human-code
  sourceWrites:
    mode: pull-request
```

A viewer also needs the instance `view` role. An operator also needs `operate`
(or `admin`). Operators have viewer access. Actions are an explicit allowlist;
empty actions enable only permission inspection. Omitted `sourceWrites` still
enforces pull-request publication. Direct repository publication is unsupported.

## Explicit execution identities

```yaml
# instance.yaml; these are independent from repos and automation credentials.
interactiveCredentials:
  - name: human-issues
    provider: github
    owner: acme
    repository: issues
    token: {env: HUMAN_ISSUES_TOKEN}
  - name: human-code
    provider: github
    owner: acme
    repository: web
    token: {store: vault/human-code-token}
  - name: human-ado-backlog
    provider: ado
    owner: organization
    project: Work Project
    token: {env: HUMAN_ADO_TOKEN}
```

These named sources reuse existing token references and GitHub App / ADO
identity authentication. They do not introduce a separate secret store. ADO
project-only backlog sources omit `repository`; repository access requires an
exact repository. GitHub requires `repository` and forbids `project`.

The backlog and every repository select their own named source. There is no
fallback to the automation identity, first repository, `connectionRef`, or a
credential from another target. Repository identities must belong to the
configured gaggle. The existing supported provider topology remains unchanged:
ADO code may use a GitHub backlog; an ADO backlog derives its organization from
an ADO code project. A GitHub owner is never inferred to be an ADO organization.

Do not add human credential environment variables to `runner.envPassthrough`.
The configuration rejects that overlap. Resolved secrets are registered with the
daemon scrubber before use and never appear in the permissions response.

Applied policy reload is fenced against each bounded provider effect. New
operations use the new grants once the scheduler catalog is published; a failed
catalog publication retains the previously applied policy. Credential selection
is rechecked for every operation. Instance credential-source changes require
restarting the daemon, like other instance configuration changes.
