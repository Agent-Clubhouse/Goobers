# #7761: Carry per-attempt usage allowance in the agentic kit

Growth: +6 non-test lines and +0 production files in `cmd/goobers`.

The agentic kit writer copies the dispatcher attempt's trusted remaining
allowance into the content-digested kit. The allowance type, validation, and
arithmetic live in internal packages; command-package code owns only this
existing kit-composition boundary.
