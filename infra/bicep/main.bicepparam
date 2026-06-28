// Example parameters for a Goobers instance bootstrap.
// Copy + edit per instance. Non-secret values live here; the PostgreSQL admin
// password is pulled from a pre-existing *bootstrap* Key Vault via getSecret so it
// is never stored in the repo (SEC-010, CFG-009).
using './main.bicep'

param namePrefix = 'goobers'
param environment = 'dev'
param location = 'eastus'

// Replace <bootstrap-sub-id> / <bootstrap-rg> / <bootstrap-kv> with the team's
// pre-provisioned bootstrap vault holding the seed secret. This is resolved at
// deploy time by the release pipeline's identity; the value never lands in git.
param postgresAdminPassword = az.getSecret(
  '<bootstrap-sub-id>',
  '<bootstrap-rg>',
  '<bootstrap-kv>',
  'temporal-postgres-admin-password'
)
