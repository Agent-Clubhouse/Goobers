# HA-P3 cloud object-store composition

Issue #7813 adds the operator-configured Azure Blob endpoint and container to
the API/controller composition root. Azure create-only storage, digest
verification, PostgreSQL surrender rows, config-generation pointers, pins, and
monotonic apply behavior remain under `internal/cloud/objstore`; command growth
is limited to dependency construction and passing the existing store seams to
the role host.
