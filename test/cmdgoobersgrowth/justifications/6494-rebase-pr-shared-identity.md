# #6494: one comment classifier for pr-comment-watch, rebase-pr and update-behind-pr

Growth: about +200 non-test lines and +1 file in `cmd/goobers`.

## Why the growth belongs in the command package

- **`commentorigin.go` (new file).** Most of it is code moved out of
  `prcommentwatch.go` and `prcommentwatch_forge.go` unchanged: the identity
  modes, the classifier, the marker check and the acknowledgement watermark.
  The move lets the three stages share it. The new lines are:
  - the shared `identityMode`/`excludeAuthors` settings reader;
  - the two identity constructors;
  - the bookkeeping-payload rule (a Goobers state comment is neither a
    person's feedback nor an answer to it).

  All of these are stage-level policy over provider-neutral comments.
- **`rebasepr.go`.** The ADO transport now reads thread comments and resolves
  the identity by id. There is also the human-comment detection helper, with
  its fail-closed handling of an unreadable comment, and the acknowledgement
  arm. These are rebase-pr's own decisions.
- **`updatebehindpr.go`.** The API-only lane no longer clears
  `needs-remediation` over unanswered human feedback: one predicate plus the
  settings read.
- **Help text.** The new inputs are documented on each stage.

## Could any of it live elsewhere?

The classifier could move to a small `internal/` package if a fourth consumer
appears outside `cmd/goobers`. Today every caller is a stage command in this
package, and its inputs (`providerInput`) and the marker helpers it reads live
here too. The provider-side piece, `ErrUnreadableFeedbackComment`, already
lives in `providers`. The cross-stage settings check (WF029) lives in
`api/validate`.
