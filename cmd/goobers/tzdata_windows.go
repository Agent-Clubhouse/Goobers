package main

// Windows containers ship no IANA time zone database, and Go's time package
// falls back to the OS registry, which does not carry IANA names. So any
// instance whose `timezone:` is a location name — which is what the field
// documents itself as ("an IANA location name (e.g. America/New_York)") —
// fails to load at all:
//
//	worker: load instance config: C:\instance\instance.yaml:
//	timezone "America/Los_Angeles": unknown time zone America/Los_Angeles
//
// Only UTC works, because Go special-cases it.
//
// That is a plain defect on Windows, and it turned load-bearing the moment a
// mixed-OS fleet appeared: a Linux instance config and a Windows worker must
// agree on one instance.yaml, so a timezone the Windows binary cannot parse
// makes the fleet unrunnable rather than merely inconvenient. Observed exactly
// that way — the Windows worker refused to start against the Linux instance's
// config while the Linux daemon ran on it happily.
//
// Embedding Go's copy of the database fixes it permanently for ~450 KB, and
// only on Windows: every other platform keeps using the system zoneinfo it
// already has.
import _ "time/tzdata"
