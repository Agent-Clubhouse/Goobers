# Invoice investigation workspace

This disposable repository is the default workspace for the interactive prototype.
It contains a real failing Node test and a small bug: discounts larger than the
subtotal must produce a zero total, never a negative invoice. `node --test` needs
no packages or network. The ADO bug and App Insights observations shown alongside
it are controlled local fixtures, not live provider responses.

Try **investigate**, **fix**, **test**, **review**, or **design workflow** in the
local-tools executor. That executor is a deterministic tool driver, not an LLM.
The optional Codex executor handles ordinary natural-language requests using the
existing Goobers harness adapter. All edits stay in the session's private copy.
