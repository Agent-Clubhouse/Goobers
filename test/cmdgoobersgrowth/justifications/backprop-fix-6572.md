# backprop/fix-6572: quiet expected Windows pod advisories

Growth: +19 non-test lines and +0 files in `cmd/goobers`, in
`dispatchcheckout.go`.

## Why the growth belongs in the command package

Pod checkout used to try `git clone --branch <workspace-branch>` first and
treat any failure as "branch does not exist yet". On the first stage of every
run that failed by design and printed a misleading clone error in the pod log.
`remoteBranchExists` now asks the remote with `git ls-remote --exit-code`
first:

- Exit code 2 means the branch genuinely does not exist.
- Any other failure, such as a bad credential or a network fault, is returned
  as the real cause instead of being guessed at.

The rebound-branch refusal still fails closed. The probe uses the same
`composeGitEnv` credentials as the checkout, which is why it lives next to
the checkout.

## Could any of it live elsewhere?

A shared git helper package could host `remoteBranchExists`. Checkout is its
only caller today.