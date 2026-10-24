// Package remediationselect is the pure selection logic shared by the
// pr-remediation and pr-select stages, the local schedule-demand counter, and
// the cloud counter that must size a scheduled remediation fire to exactly what
// the stage will select (#8131).
//
// It owns the vocabulary the sticky PR comments carry (remediation causes and
// outcomes, attempt counters, the remediation-state, blocked-on-sibling and
// merge-demotion payloads and their parsers), the live-state decisions over them
// (is a recorded escalation still blocking, is a named sibling still live, does
// a demotion still hold), and the candidate selection itself (exclusions,
// priority tiers, behind-base, claim and live-origin availability).
//
// It is forge-agnostic: it reads through the narrow Provider interface and takes
// the claim ledger listing and the cross-run journal reader as arguments. Stage
// commands, flag parsing, provider and credential construction, and the claim
// ledger's transactions stay in cmd/goobers, which re-exports the moved names
// under their original unexported spellings so its call sites are unchanged.
package remediationselect
