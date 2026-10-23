# Vendored CRD schemas

These are the JSON schemas `make deploy-validate` uses to validate operator-owned
kinds with kubeconform, so CRD validation needs no network. They are
byte-identical copies of the files in
[datreeio/CRDs-catalog](https://github.com/datreeio/CRDs-catalog) at commit
`501527ab9c40c827144c8f45945b25a023e344ce`. The layout,
`<group>/<kind>_<version>.json`, matches kubeconform's schema-location template.

The `postgresql.cnpg.io` schemas at that commit are CloudNativePG **1.29.0**, so
`make deploy-validate` checks the CNPG reference against the 1.29 API. The
reference documents a minimum operator of **1.26** (the first release with
`dataDurability`; see `deploy/reference/goobers-cloud/cnpg/README.md`). Nothing
here validates against 1.26–1.28, so a field the reference uses that is newer
than its documented minimum would still pass. The `barmancloud.cnpg.io` schema
comes from the Barman Cloud plugin, not the operator release.

| File | SHA-256 |
|---|---|
| `barmancloud.cnpg.io/objectstore_v1.json` | `bc4e8f8ad33e925b190e0697c49917193193d6d9f0324e2a02fb91152ce249ff` |
| `postgresql.cnpg.io/cluster_v1.json` | `b58695a447c9be7cc5e49faeba0280ec13a34a7b067ce26ed9f8d616717df960` |
| `postgresql.cnpg.io/database_v1.json` | `4536480e580227a97c54f730bdd299c332af5a84b72a25261e741bdd4e3b6495` |
| `postgresql.cnpg.io/scheduledbackup_v1.json` | `975bdd33e518a9bf45c50703cff4c6843daa62d68fc5ea164f69c4618d344657` |

`TestVendoredCRDSchemasMatchRecordedChecksums` (in `test/ci`) fails if a file
changes without its row here. To update, download the new commit's files over
these, then update the commit and checksums in this README.
