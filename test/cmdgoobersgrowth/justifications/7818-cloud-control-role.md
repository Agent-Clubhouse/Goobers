# HA-P3-18 cloud control composition

The command adds control-role flag parsing, PostgreSQL/Temporal/auth construction,
HTTP plane registration, probes and process shutdown. Those are composition-root
responsibilities. Lease lifecycle, acceptance dispatch/recovery and durable
provider observations live in internal/cloud/control. No local command grows;
no runner or filesystem-backed cloud fallback is introduced.
