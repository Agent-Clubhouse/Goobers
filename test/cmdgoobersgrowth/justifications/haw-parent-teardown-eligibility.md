# HAW parent teardown eligibility

The command layer already owns authenticated parent pod scope selection and
host journal composition. This change factors its existing custody verifier
so the HTTP wrappers can check one exact unjoined physical contract after a
stage ends. It does not grant credentials, new execution, or sibling access.
The bounded stores and worker transport remain in internal packages.
