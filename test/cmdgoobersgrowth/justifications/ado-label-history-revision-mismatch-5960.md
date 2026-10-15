# ADO label-history revision mismatch diagnostic: #5960

Growth: +29 non-test lines and +0 files in `cmd/goobers`, all in
`backlogquery.go`. The new code is `reportRevisionMismatch` and its
annotation constant.

## Why the growth belongs in the command package

The ADO ready-age updates walk (`providers`) now fails closed with
`providers.ErrLabelHistoryRevisionMismatch` when an item's update history
ends before its current revision. That check rests on ADO writing exactly
one update per revision. If the assumption were ever wrong, backlog-query
would skip every ADO claim, one item at a time.

The skip happens in backlog-query's claim session (`skipMalformedClaim`),
and that is where this code goes. The reaction to the provider's typed
error is what makes such a failure visible:

- a stable error code on stderr;
- an annotation through the session's instance-journal seam.

Both the claim session and the journal seam exist only in this package.

## Could any of it live elsewhere?

The error type and its code already live in `providers`. Only the reaction,
the stderr line and the journal annotation, is here. Moving it out would mean
moving the claim session too.
