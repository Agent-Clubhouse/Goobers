# Control configuration assembly seams (#7818, L1d)

Growth: +13 non-test lines and +0 files in `cmd/goobers-cloud`, in
`controlassembly_config.go`.

## Why the growth belongs in the command package

The lines split the control role's configuration assembly into two small
constructors, `controlDirSource` and `newControlConfiguration`, that
`constructConfiguration` now calls. They contain no new behaviour: they hold the
two composition decisions (the composed assessor, and the admission pins computed
from the generation's own tree) in one place each, so a test can build through
the same constructors the control role uses. Dropping either decision in a future
merge would refuse every start or publish ineligible generations, and a
one-line argument in a large function is easy to lose; the tests that now pin
them are in `controlassembly_config_test.go`.

The domain logic stays where it was: pin computation in
`internal/cloud/startpin`, eligibility in `internal/cloud/config/readiness` and
`internal/cloud/schedentries`.

## Could any of it live elsewhere?

The two constructors are composition-root policy (which concrete assessor and
pin function the control role hands the config source), so they belong here. A
second composition root would be the reason to move them.
