# Azure DevOps onboarding example

Scaffold a complete instance, including the provider-neutral workflows and their
required credential grants:

```sh
goobers init --template=standard --provider=ado --ci-command='["dotnet","test"]' --required-capabilities=dotnet@8 ./ado-instance
goobers connect contoso/platform/widgets ./ado-instance
```

Replace `contoso/platform/widgets` with your real organization/project/repository.
The .NET command is an example, not an ADO requirement. Set `GOOBERS_ADO_TOKEN`
securely in your environment; the instance records only the variable name.

[`gaggle.yaml.example`](gaggle.yaml.example) is a complete gaggle for these exact
example coordinates. Copy it to `ado-instance/config/gaggles/example/gaggle.yaml`
and adapt its repository coordinates, Boards project, branch and toolchain.
Keep the generated manifest, workflows, goobers and instance credential grants;
the gaggle alone is not a complete instance.

The corresponding repository entry in `instance.yaml` is:

```yaml
repos:
  - provider: ado
    owner: contoso
    project: platform
    name: widgets
    auth:
      kind: pat
    token:
      env: GOOBERS_ADO_TOKEN
```

The Boards `backlog.project` can differ from the repository project. Check both
Git and Boards access, then optionally seed one tagged Task:

```sh
goobers validate --strict --check-repos ./ado-instance
goobers connect contoso/platform/widgets --seed ./ado-instance
```

See [production onboarding](../../docs/guides/arbitrary-repo-onboarding.md#3-initialize-the-instance)
for seeding bounds and tag semantics, and [ADO authentication](../../docs/guides/ado-authentication.md)
for PAT scopes, other authentication sources and PR-status evidence.
