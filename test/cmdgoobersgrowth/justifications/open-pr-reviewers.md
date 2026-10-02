# cmd/goobers growth: open-pr reviewers

Adds the optional `reviewers` input to the existing `open-pr` stage command (openpr.go): input parsing, a best-effort RequestReview call after the PR opens, result-file fields and help text. The growth is entirely inside that stage command; the provider call itself already existed in providers/.
