package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/boundedwait"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/ephemeraltmp"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/platform/proc"
	"github.com/goobers/goobers/internal/providerstage"
	"github.com/goobers/goobers/internal/telemetry"
	"github.com/goobers/goobers/providers"
)

// DefaultTimeout bounds a shell stage's execution when neither the executor
// nor the stage declares one.
const DefaultTimeout = boundedwait.DefaultTimeout

// DefaultMaxOutputBytes caps captured stdout/stderr (each stream) when
// neither the executor nor the stage declares a limit.
const DefaultMaxOutputBytes int64 = 1 << 20 // 1 MiB

// groupKillWaitDelay bounds how long Run waits for cmd.Wait() to return
// after killing the whole process group on timeout, in case a descendant
// escaped the group (e.g. via setsid) and is still holding a stdout/stderr
// pipe open — cmd.Wait() would otherwise never return, hanging the stage
// (and graceful drain) exactly as before the group-kill fix, just one layer
// down (#119). Giving up after this bound lets the stage's own accounting
// proceed even though the escaped process may leak; there is no portable,
// unconditional way to guarantee its death. Mirrors internal/harness's
// identical constant (a second, small copy — not worth a shared package for
// one duration value, same tradeoff already accepted for fsyncDir this
// wave).
const groupKillWaitDelay = 5 * time.Second

// Provider reset timestamps are second-granularity and may be slightly ahead
// of the runner's clock.
const providerRateLimitResetSlack = 2 * time.Second

// timeoutDumpGrace bounds how long Run waits, after sending SIGQUIT to a
// timed-out stage's process group, for the Go processes in it (go test, the
// goobers CLI) to write their FULL goroutine traces to the
// captured stdout/stderr and exit before Run escalates to SIGKILL. Go's
// default SIGQUIT handler dumps every goroutine's stack (regardless of
// GOTRACEBACK level) and exits — so on the one path that matters, a stage that
// blew its timeout, this turns an opaque "killed at 10m, no output" record
// into a self-diagnosing artifact showing exactly which goroutine/test was
// blocked and on what. It costs nothing on the happy path (only a timed-out
// stage reaches here) and never removes the SIGKILL backstop below: a process
// that ignores SIGQUIT (a non-Go child, one that installed a handler, or one
// wedged in an uninterruptible syscall) is force-killed after this grace.
const timeoutDumpGrace = 5 * time.Second

// Well-known Task.Inputs keys a deterministic shell stage may declare. These
// travel through InvocationEnvelope.Inputs rather than as DeterministicRun
// fields — see doc.go.
const (
	// InputTimeout is a time.ParseDuration string, e.g. "5m".
	InputTimeout = boundedwait.InputTimeout
	// InputResultFile is a path, relative to the workspace, whose bytes (once
	// the command exits) become an artifact. If declared, the file's presence
	// is also a success criterion: a zero exit with no such file is a failure.
	// If those bytes also parse as a flat JSON object, its string/number/bool
	// fields are additionally merged into ResultEnvelope.Outputs (in addition
	// to, not instead of, recording the raw bytes as an artifact) — this is
	// how a shell subcommand (a real OS subprocess, not an in-process
	// invoke.Deterministic) reports structured handoff data a downstream
	// task's Task.InputsFrom can reference, e.g. `goobers open-pr`'s prNumber
	// (#132). Not JSON, or not a flat object, is not an error: the artifact/
	// presence-check contract holds regardless. Registered provider stages get
	// their command-specific default when the workflow omits this input.
	InputResultFile = "resultFile"
	// InputMaxOutputBytes is a decimal integer overriding the per-stream
	// output cap.
	InputMaxOutputBytes = "maxOutputBytes"
)

// OutputNoWork is the well-known InputResultFile output key a deterministic
// command sets to boolean true to report ResultNoWork instead of
// ResultSuccess (issue #233): the command exited 0 (it did not error) and
// its declared result file was present and parsed as JSON, but it found
// nothing to act on this tick (e.g. `goobers backlog-query --claim` with an
// empty or fully-contested eligible set). Checked only after a successful
// declared-result-file read, so this is an explicit, structured signal, not
// an exit-code convention every unrelated shell stage would have to avoid
// colliding with. A command with no declared InputResultFile has no way to
// signal ResultNoWork — only ResultSuccess (exit 0) or ResultFailure — since
// there is nowhere else fail-closed to read a structured signal from.
const OutputNoWork = "noWork"

// OutputErrorCode / OutputErrorMessage / OutputErrorRetryable are the
// well-known InputResultFile output keys a deterministic command sets to
// report a TYPED failure — the failure analog of OutputNoWork (#614). A
// command that exits nonzero after writing its declared result file with
// OutputErrorCode set gets that code (and message/retryable, when present)
// as the stage's ErrorInfo instead of the generic nonzero_exit — and,
// because the file exists, instead of the missing_result_file that used to
// bury the real cause (e.g. a GitHub rate-limit 403 now journals as
// github_rate_limited with the reset time in its message). Checked only on
// a nonzero exit with a successfully read result file, so an unrelated
// stage that never writes these keys keeps exactly the old behavior.
const (
	OutputErrorCode      = "errorCode"
	OutputErrorMessage   = "errorMessage"
	OutputErrorRetryable = "errorRetryable"
)

// ArtifactRecorder persists stage output bytes into the run journal and
// returns a content-addressed pointer to them. *journal.Run satisfies this.
type ArtifactRecorder interface {
	RecordArtifact(name string, data []byte) (journal.Ref, error)
}

// BoundedArtifactRecorder applies a byte limit to the final persisted artifact.
type BoundedArtifactRecorder interface {
	ArtifactRecorder
	RecordArtifactBounded(name string, data []byte, maxBytes int) (journal.Ref, error)
	RecordArtifactBoundedWithIntegrity(name string, data []byte, integrity apiv1.Integrity, maxBytes int) (journal.Ref, error)
}

type integrityArtifactRecorder interface {
	RecordArtifactWithIntegrity(name string, data []byte, integrity apiv1.Integrity) (journal.Ref, error)
}

// ShellExecutor runs deterministic shell stages (invoke.Deterministic) in the
// worktree the caller hands it via InvocationEnvelope.Workspace.
type ShellExecutor struct {
	// Injector resolves capability-scoped credentials for a stage's declared
	// capabilities. Required.
	Injector *credentials.Injector
	// Journal records captured output and declared result files as
	// content-addressed artifacts. Required.
	Journal ArtifactRecorder
	// DefaultTimeout overrides the package DefaultTimeout when positive.
	DefaultTimeout time.Duration
	// DefaultMaxOutputBytes overrides the package DefaultMaxOutputBytes when
	// positive.
	DefaultMaxOutputBytes int64
	// InstanceRoot, if set, is passed to every stage process as
	// GOOBERS_INSTANCE_ROOT — the only way a `goobers` CLI subcommand invoked
	// as a stage's command (its cwd is the stage's worktree, not the instance
	// root) can locate instance.yaml/config/scheduler (#131/#132). Empty by
	// default: a caller that never sets it (e.g. an existing test) gets
	// unchanged behavior — no such var is set.
	InstanceRoot string
	// SelfBin, if set, is the absolute path substituted for a bare "goobers"
	// command token before exec. Deterministic stages declare their command as
	// e.g. ["goobers", "backlog-query", …], but a stage runs with cwd set to a
	// fresh worktree clone that never contains the (gitignored, uncommitted)
	// goobers binary, and a bare name is PATH-resolved against the *daemon's*
	// PATH — not the worktree — so "goobers" fails at exec (#229). Wiring sets
	// this once from os.Executable() so a stage execs the exact same binary as
	// the running daemon (no version skew). Empty by default: an unset caller
	// runs the command verbatim (unchanged behavior).
	SelfBin string
	// Diagnostics, when true (goobers up --diagnostics), arms a per-stage
	// watchdog: any stage still running past diagnosticsSampleAfter gets a
	// periodic native process sample + process tree + open-fd (lsof) snapshot
	// recorded as a run artifact. This is the capture that actually works on a
	// wedged `go test -race` local-ci stage — SIGQUIT/-test.timeout can't dump
	// it (the race runtime can't stopTheWorld while a goroutine is stuck in a
	// syscall), but an OS-level sample shows the blocked threads regardless.
	// Off by default: zero cost and no extra files unless explicitly enabled.
	Diagnostics bool
	// ExtraEnvAllowlist names additional ambient env vars carried into every
	// stage subprocess on top of the built-in procenv default-deny allowlist —
	// the instance's RunnerConfig.EnvPassthrough (#736), for a custom toolchain
	// whose env var the built-in list does not cover. Empty by default: an
	// unset caller gets the built-in allowlist unchanged.
	ExtraEnvAllowlist []string
	// DefaultEnv supplies runner-owned stage defaults. A stage's explicitly
	// declared run.env values override matching keys.
	DefaultEnv map[string]string
	// ScratchDir, if set, roots the built-in error file this executor creates
	// for every goobers-CLI stage (BuiltinErrorFileEnvVar) instead of the OS
	// default temp directory. Wiring sets this to the same already-writable
	// scratch directory the runner uses for scratch-mode workspaces
	// (under the instance's workcopies root), which — unlike the OS default
	// temp dir — is guaranteed to exist and be writable on any instance that
	// runs at all, independent of whether the process environment happens to
	// set TMPDIR or the container happens to mount something at /tmp. A
	// read-only-root deployment that mounts nothing at /tmp and sets no
	// TMPDIR previously failed here with "open /tmp/goobers-builtin-error-…:
	// read-only file system" on the first stage that errored (#3342) — the
	// path is exercised only when a goobers-CLI stage reports a typed
	// failure, so it validated and booted clean and only broke later. Empty
	// by default: an unset caller (e.g. an existing test) gets unchanged
	// behavior — os.CreateTemp("", ...) resolves against os.TempDir(), which
	// already honors TMPDIR when the process environment sets it.
	ScratchDir string
	// EphemeralTmp binds the `tmp:ephemeral` restriction on the SELF runner
	// (docs/design/goobernetes-restrictions.md §2.4, the modes-1/2 half of the
	// effect the dispatcher gives a stage pod by construction). When set,
	// every stage this executor runs gets an attempt-private temp directory
	// carved out of the daemon's temp root — TMPDIR/TMP/TEMP pointed at it,
	// every temp-nested build cache (GOCACHE, GOMODCACHE, ...) re-rooted into
	// it — and that directory is destroyed when the attempt returns, on the
	// failure path as much as the success path.
	//
	// It is a RUNNER property, not a stage requirement: wiring sets it from
	// the resolved inventory's self entry declaring the effect, and then every
	// stage placed on self runs under it whether or not it asked
	// (goobernetes-restrictions.md §5). Off by default, so an instance that
	// declares no runners — or a self entry that declares no restrictions —
	// builds a byte-identical stage environment to before this field existed.
	//
	// The failure mode is CLOSED. If the private directory cannot be
	// established the stage fails with a named diagnostic rather than running
	// against ambient temp, because a restriction that silently degrades is
	// worse than one that is absent: the solver has already told the operator
	// this runner enforces it.
	EphemeralTmp bool
	// EphemeralTmpRoot overrides the temp root EphemeralTmp carves the
	// per-attempt directory out of. Empty means the daemon's own temp root
	// (os.TempDir(), which honors TMPDIR) — deliberately the SAME medium the
	// stage's temp would otherwise land on, so binding the effect changes the
	// lifetime of those bytes and not their location. Set by tests, and
	// available to a deployment that mounts its scratch medium elsewhere.
	EphemeralTmpRoot string
}

type builtinErrorReport struct {
	Code      string `json:"errorCode"`
	Message   string `json:"errorMessage"`
	Retryable bool   `json:"errorRetryable"`
}

// NewShellExecutor builds a ShellExecutor. injector and journal must not be
// nil: a nil injector could silently skip capability admission, and a nil
// journal would leave captured output unrecorded — both fail closed here
// rather than at first use.
func NewShellExecutor(injector *credentials.Injector, rec ArtifactRecorder) (*ShellExecutor, error) {
	if injector == nil {
		return nil, errors.New("executor: injector must not be nil")
	}
	if rec == nil {
		return nil, errors.New("executor: journal must not be nil")
	}
	return &ShellExecutor{Injector: injector, Journal: rec}, nil
}

// StageInvokesGoobersCLI reports whether a stage's command is the goobers CLI
// itself (e.g. backlog-query/open-pr/ci-poll/issue-close-out) rather than an
// external tool (make, go, git). It is the single discriminator for two
// goobers-CLI-specific behaviors: substituting the daemon's own binary for the
// bare "goobers" token (SelfBin, #229), and injecting the run's operational
// identity into the stage env (#322). A stage that runs the project's own
// build/test suite (`make ci`) is not a goobers-CLI stage on either axis.
func StageInvokesGoobersCLI(command []string) bool {
	return len(command) > 0 && command[0] == "goobers"
}

// stageCommandsRequiringInstanceConfig are goobers subcommands that read the
// instance CONFIG DIRECTORY — the workflow/gaggle definitions — not just the
// routed repo and a credential. A stage pod has no config directory, so these
// cannot run there.
//
// DERIVED, and re-derivable: `grep -l LoadConfigDir cmd/goobers/*.go`, then map
// each file to the command its newCLIFlagSet declares. That yields 16 files,
// all of which are operator commands (config, connect, fix, features,
// onboarding, run, workflow) that a workflow never invokes as a stage. If that
// grep ever names a new STAGE command, it belongs here.
//
// The map is EMPTY as of Goobers#4001. Its only entry was telemetry-query,
// which now reads the daemon's bounded defect-aggregate plane in a dispatched
// pod (internal/httpapi/telemetrydefectplane.go) and only consults the config
// directory on the local path, which it refuses to take without a real
// instance root. The map is kept rather than deleted because the CONDITION it
// encodes is still real: a stage pod has no config directory, and the next
// command that needs one belongs here.
var stageCommandsRequiringInstanceConfig = map[string]bool{}

// StageRequiresInstanceConfig reports whether a stage command needs the instance
// config directory, and so cannot run in a stage pod. This is deliberately a
// NARROW list rather than "every goobers CLI stage": measured against the
// commands this instance's workflows actually invoke, all the rest reach their
// repo and credential through providerRepo/providerToken, both of which read
// the environment first — which is exactly what the pod stamps.
func StageRequiresInstanceConfig(command []string) bool {
	if !StageInvokesGoobersCLI(command) || len(command) < 2 {
		return false
	}
	return stageCommandsRequiringInstanceConfig[command[1]]
}

// stageCommandsRequiringInstanceRoot are goobers CLI subcommands that
// UNCONDITIONALLY read or write state that lives only under the daemon's
// instance root — a file this process opens by path, under a lock this
// process takes, that no plane serves. A stage pod stamps no
// GOOBERS_INSTANCE_ROOT (internal/dispatcher/podspec.go), so
// providerStageRoot() falls back to "." and these commands would silently
// operate on an empty, pod-local root instead of failing — the exact
// "silent-wrong-result" class this refusal turns loud.
//
// THE LIST SHRANK WITH Goobers#3897/#3898. Four plane clients were already
// landed (claims C1, scheduler-state C2, telemetry C3, journal-read C4) but
// every one of them selects its backend from environment the dispatcher did
// not stamp, so a mode-3 stage silently took the local-file branch and the
// refusals had to stay. #3897 stamps the complete eight-variable set
// (endpoint + bearer for all four planes, alongside GOOBERS_RUN_ID and
// GOOBERS_GAGGLE) into every goobers-CLI stage pod, and #3898 moved the
// claiming path's last two local dependencies — the instance-log annotation
// write and the backlog re-sweep state file — onto the journal emit plane and
// the scheduler-state key namespace respectively.
//
// The removals were made ONE COMMAND AT A TIME against a per-command audit:
// a command was removed only when EVERY stateful access it makes, followed
// transitively from its entry function, reaches a plane seam
// (openStageClaimLedger, openStageStateStore/openHeldStageStateStore,
// stageRunJournal/stageCrossRunJournal, openStageAnnotator,
// stageImplementationOutcomes), a provider API call, or a workspace-relative
// file. Anything still holding a path under the instance root is BELOW, with
// the specific file named — because trading a loud refusal for a silent wrong
// answer is strictly worse than the refusal.
//
// backlog-query and backlog-health used to be singled out here as
// flag-gated: only specific FLAGS made them provider-only rather than
// ledger-touching, so StageRequiresInstanceRoot matched them by name. Both
// are now fully plane-served in every mode (#3898, #3948) and neither appears
// in the map below at all. pr-select left it the same way (#3988), by
// admitting its fairness lease to the scheduler-state namespace.
//
// Scope: this matches on the COMMAND VECTOR (cmd[0]=="goobers", cmd[1]=the
// subcommand), the same shape both dispatchRemoteTask and the pod-entrypoint
// backstop pass in (t.Run.Command / DeterministicCommand's argv). A stage
// declared with run.script instead of run.command is out of scope on both
// sides today — DeterministicCommand's argv for a script is the shell
// wrapper, never the goobers invocation inside it — matching the
// pre-existing, narrower StageRequiresInstanceConfig's scope.
//
// DERIVED, and re-derivable: per command, follow its handler transitively and
// grep for SchedulerDir()/journal.OpenInstanceLog/journal.OpenRead/
// withClaimLock/withFileLock/localscheduler.OpenClaimLedger/TelemetryDB()/
// RunsDir()/FindRunDir/instance.LoadConfig that is NOT behind a plane seam or
// a plane predicate (statePlaneSelected/claimsPlaneSelected/
// journalPlaneSelected). Re-run that walk against every registered
// stageCommand() (cmd/goobers/runtime_capabilities.go) before trusting this
// list is still complete.
var stageCommandsRequiringInstanceRoot = map[string]bool{
	// pr-select is NOT here any more (Goobers#3988). Its claims are on the
	// claims plane and the last thing that held it — the FAIRNESS LEASE at
	// SchedulerDir()/pr-select-fairness.json, #1336's aging plus the one-hour
	// starvation guard — is now a scheduler-state key
	// (stateclient.KeyPRSelectFairness), reached through
	// openStageStateStore/openHeldStageStateStore like every other key in that
	// namespace and served under the SAME claims.lock it always took, so a
	// pod-executed selection and a daemon-driven one advance ONE lease rather
	// than two. Everything else it touches is a provider API call.
	// issue-close-out reads run journals through journal.OpenRead directly
	// (issuecloseout.go:96, :168, :241) over a run directory it finds with
	// instance.Layout.FindRunDir — bypassing the stageRunJournal seam
	// entirely, so the journal plane does not serve it. Its claim RELEASE is
	// already plane-served; only these three reads hold it here.
	"issue-close-out": true,
	// telemetry-query is NOT here any more (Goobers#4001). It opened the
	// daemon's telemetry ROLLUP database directly (l.TelemetryDB()), and
	// decision 005 R4 refused it at dispatch because the only shape anyone
	// had for serving it would have exposed that database or raw error
	// signatures. It now reads a NARROW, gaggle-contained, run-authenticated
	// aggregate route instead (apicontract.TelemetryDefectAggregatesPath):
	// four fixed derived families, no SQL, no path, no connector, and error
	// signatures normalized before they leave the daemon. The command selects
	// that plane BEFORE resolving a root, and on the local path it now
	// refuses outright when the resolved root is not an instance — so the
	// silent "." fallback this entry existed to prevent is unreachable from
	// either direction rather than merely undispatched.
	// gather-pr-context is NOT here any more (Goobers#3989). Its REMEDIATION
	// NO-OP GUARD — the record that stops the lane re-attempting a PR whose
	// previous attempt already concluded there was nothing to do — held it
	// three separate ways, and all three are now plane seams
	// (remediationnoopguard.go): the record is a keyed scheduler-state key
	// (stateclient.PRRemediationNoopKey, one per gaggle+PR) reached through
	// openStageStateStore, its claim resolution goes through the claims seam
	// (stageClaimLedgerForRun/Locked) instead of localscheduler.OpenClaimLedger,
	// and the terminal run's journal is read through stageRunJournal instead of
	// journal.OpenRead over a FindRunDir path. claims.lock mutual exclusion is
	// preserved: every no-op key falls through schedulerStateLock's default
	// arm, which is claims.lock, and the daemon serves a pod's compare-and-swap
	// under that same lock. remediation-checkpoint, which shares the guard,
	// clears with it.
	// select-source opens the instance log (selectsource.go:99), walks the
	// instance's runs tree through readservice.NewOfflineRuns (:89), and
	// leases the parent with withClaimLock + localscheduler.OpenClaimLedger
	// directly (:219-224, :240-243) rather than through the claims seam.
	"select-source": true,
	// publish-batch leases decomposition targets with a FileTargetLeaser over
	// SchedulerDir()/decomposition-target-locks (publishbatch.go:116), opens
	// the instance log (:145), and shares select-source's direct parent
	// release (:154). The target-lock directory has no plane at all.
	"publish-batch": true,
	// backlog-health is NOT here any more (Goobers#3948). Its claim read is on
	// the claims plane, its implementation-outcome evidence read is on the
	// telemetry plane, and the last thing that held it — the READY-TRANSITION
	// LEDGER, the resumable label-transition scan cursor at
	// instance.Layout.BacklogHealthCursorPath — is now a scheduler-state key
	// (stateclient.BacklogHealthCursorKey), reached through
	// openStageStateStore like every other key in that namespace. Both modes
	// are dispatchable: the bare snapshot and --feedback share one ledger
	// resolution, so nothing about the flag changes which state is touched.
	// Its provider read-cache writes are the same correctness-neutral
	// conditional-GET store backlog-query and backlog-dedupe already carry
	// into a pod.
	// reconcile-branches opens the instance log (reconcilebranches.go:155)
	// and reads OTHER runs' journals by walking layout.RunsDir() with
	// journal.OpenRead (:166, :452) — a cross-run walk the journal plane's
	// three purpose-built gaggle-scoped questions do not answer.
	"reconcile-branches": true,
}

// stageKindsWithPodExecution names the built-in deterministic stage KINDS
// that HAVE a pod-side execution path, so a stage declaring one is not
// refused by StageRequiresInstanceRoot's kind arm.
//
// AN ALLOWLIST, NOT A DENYLIST, and that direction is the point: an
// unrecognized kind — a newer engine dispatching a kind this binary has never
// heard of — falls through to `true` and is refused, rather than being
// dispatched into a pod whose dispatch-exec has no branch for it and would
// silently run the stage's PLACEHOLDER command instead (implementation.yaml's
// ["goobers","ci-poll"] exits nonzero; a future kind's placeholder might exit
// 0 and surrender an empty success). Adding a kind here is therefore a
// deliberate act taken together with the in-pod branch that runs it.
//
//   - KindShell is the ordinary shell-command case (kind == "" means the
//     same); it has always run in a pod.
//   - KindCIPoll runs in-process inside dispatch-exec (decision 005 C5,
//     #3881): cmd/goobers/dispatchcipoll.go builds a CIPollExecutor with
//     provider:pr:write resolved through the credential plane, exactly as
//     every other pod stage resolves its declared capabilities. It needs no
//     ledger, no merge lock and no on-disk journal — only a provider token —
//     which is why it is the one kind that could leave this refusal.
//
// KindExternalTelemetry is deliberately ABSENT and stays refused: its
// executor is built from the instance's connector configuration
// (buildExternalTelemetryExecutor, cmd/goobers/runnerwiring_executors.go),
// which lives under the instance config directory a pod does not have.
var stageKindsWithPodExecution = map[string]bool{
	KindShell:  true,
	KindCIPoll: true,
}

// StageRequiresInstanceRootCode names, in a stage's failure ErrorInfo.Code,
// a refusal driven by StageRequiresInstanceRoot — shared by the engine's
// dispatchRemoteTask (refuses before a pod is ever created) and the
// pod-entrypoint backstop (cmd/goobers/dispatchexec.go, refuses in-pod on
// version skew) so the SAME failure carries the SAME name on both sides of
// the dispatch boundary, rather than two call sites inventing their own
// spellings of one refusal.
const StageRequiresInstanceRootCode = "instance_root_required"

// StageRequiresInstanceRoot reports whether a stage cannot execute in a pod
// today because it needs the daemon's instance root: either its resolved
// stage KIND is a built-in with no pod-side execution path
// (external-telemetry, or any kind this binary does not recognize — see
// stageKindsWithPodExecution; kind == "", KindShell and KindCIPoll all run in
// a pod and are never refused here), or its command is a goobers CLI
// subcommand that reads/writes the file claim ledger, a merge lock, or an
// on-disk run journal — none of which a stage pod has (decision 003 ruling 3;
// production-lanes-3.0 stillBroken #2).
//
// DELIBERATELY one data-driven list (two package-level maps), not a switch
// spread across call sites: decision 003's
// later runner branch (step 6) consumes this exact function so the two
// dispatch paths — the engine's dispatchRemoteTask and the runner's — can
// never silently diverge on which stages are refused.
//
// kind is the stage's resolved Task.Inputs["kind"]; pass "" for an ordinary
// shell-command stage.
func StageRequiresInstanceRoot(cmd []string, kind string) bool {
	if kind != "" && !stageKindsWithPodExecution[kind] {
		return true
	}
	if !StageInvokesGoobersCLI(cmd) || len(cmd) < 2 {
		return false
	}
	return stageCommandsRequiringInstanceRoot[cmd[1]]
}

// StageInvokesProviderBuiltin narrows transient stderr classification to the
// built-in stages that call a provider. Other goobers subcommands can fail
// with similar words but have separate retry contracts.
func StageInvokesProviderBuiltin(command []string) bool {
	if !StageInvokesGoobersCLI(command) || len(command) < 2 {
		return false
	}
	_, ok := ProviderStageResultFile(command[1])
	return ok
}

// ProviderStageResultFile returns the shared result-file default for a
// provider-backed workflow command. The CLI registry and shell executor both
// consume this registry so newly introduced commands cannot acquire only half
// of the command/result-file contract.
func ProviderStageResultFile(command string) (string, bool) {
	return providerstage.ResultFile(command)
}

// additionalRepoPaths projects the invocation envelope's read-only reference
// checkouts (MGV-11 #1286) into the name->path map buildStageEnv injects as
// GOOBERS_ADDITIONAL_REPO_* vars. Returns nil for a stage with none.
func additionalRepoPaths(workspaces []apiv1.AdditionalWorkspace) map[string]string {
	if len(workspaces) == 0 {
		return nil
	}
	paths := make(map[string]string, len(workspaces))
	for _, w := range workspaces {
		if w.Name == "" || w.Path == "" {
			continue
		}
		paths[w.Name] = w.Path
	}
	return paths
}

// Run implements invoke.Deterministic. It executes run.Command, or run.Script
// through the host's native command interpreter, in env.Workspace with a
// capability-scoped, non-ambient environment. It enforces a timeout by killing
// the whole process group, captures size-bounded and secret-scrubbed
// stdout/stderr as artifacts, and — if InputResultFile is declared — lifts that
// file into an artifact and requires its presence for success.
//
// A non-nil error means the executor itself could not produce a result
// (misconfiguration, credential resolution failure, a journal write failure,
// or a transient built-in provider outage) — ARCHITECTURE.md invariant 6, fail
// closed rather than degrade. Other declared-command failures are normal
// ResultFailure envelopes.
func (e *ShellExecutor) Run(ctx context.Context, env apiv1.InvocationEnvelope, run apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
	if env.Workspace == "" {
		// exec.Cmd treats Dir == "" as "run in the daemon's own working
		// directory" — a silent, surprising fallback (#122) rather than the
		// fail-closed misconfiguration error an unset workspace should be.
		return apiv1.ResultEnvelope{}, errors.New("executor: InvocationEnvelope.Workspace is empty")
	}
	command, commandEnv, cleanup, err := DeterministicCommand(run)
	if err != nil {
		return apiv1.ResultEnvelope{}, err
	}
	defer cleanup()
	timeout, err := e.timeoutFor(env)
	if err != nil {
		return apiv1.ResultEnvelope{}, err
	}
	maxOutput, err := e.maxOutputFor(env)
	if err != nil {
		return apiv1.ResultEnvelope{}, err
	}
	resultFile := stringInput(env, InputResultFile)
	implicitResultFile := ""
	if resultFile == "" && StageInvokesGoobersCLI(command) && len(command) > 1 {
		if defaultResultFile, ok := ProviderStageResultFile(command[1]); ok {
			resultFile = defaultResultFile
			implicitResultFile = defaultResultFile
		}
	}

	registry, scrubber := journal.DefaultScrubber()
	// Only a stage whose command IS the goobers CLI receives the run's
	// operational identity (GOOBERS_RUN_ID etc.). A stage that runs the
	// project's own build/test suite (local-ci's `make ci` → `go test ./...`)
	// must not inherit it, or — in a self-hosting project — the runner's live
	// run env leaks into its own test suite (#322). This is the same
	// command[0]=="goobers" discriminator the SelfBin substitution uses below:
	// the goobers-CLI-stage-ness of a stage is what decides both.
	injectRunContext := StageInvokesGoobersCLI(command)
	declaredEnv := make(map[string]string, len(e.DefaultEnv)+len(run.Env))
	for key, value := range e.DefaultEnv {
		declaredEnv[key] = value
	}
	for key, value := range run.Env {
		declaredEnv[key] = value
	}
	stageEnv, err := buildStageEnv(ctx, e.Injector, env.Capabilities, registry, env.RunID, env.Gaggle, env.WorkflowID, env.BranchNamespace, env.BaseBranch, e.InstanceRoot, injectRunContext, env.Inputs, declaredEnv, e.ExtraEnvAllowlist, additionalRepoPaths(env.AdditionalWorkspaces))
	if err != nil {
		return apiv1.ResultEnvelope{}, fmt.Errorf("executor: build stage environment: %w", err)
	}
	stageEnv = append(stageEnv, commandEnv...)
	if injectRunContext && env.TriggerRef != "" {
		stageEnv = append(stageEnv, TriggerRefEnvVar+"="+env.TriggerRef)
	}
	if injectRunContext && env.RepoRef.Provider != "" {
		stageEnv = append(stageEnv,
			RepoProviderEnvVar+"="+string(env.RepoRef.Provider),
			RepoOwnerEnvVar+"="+env.RepoRef.Owner,
			RepoNameEnvVar+"="+env.RepoRef.Name,
		)
		if env.RepoRef.Project != "" {
			stageEnv = append(stageEnv, RepoProjectEnvVar+"="+env.RepoRef.Project)
		}
	}
	if implicitResultFile != "" {
		stageEnv = append(stageEnv, InputEnvVar(InputResultFile)+"="+implicitResultFile)
	}
	builtinErrorFile := ""
	if injectRunContext {
		if e.ScratchDir != "" {
			if mkdirErr := os.MkdirAll(e.ScratchDir, 0o700); mkdirErr != nil {
				return apiv1.ResultEnvelope{}, fmt.Errorf("executor: create built-in error scratch dir: %w", mkdirErr)
			}
		}
		file, createErr := os.CreateTemp(e.ScratchDir, "goobers-builtin-error-*")
		if createErr != nil {
			return apiv1.ResultEnvelope{}, fmt.Errorf("executor: create built-in error file: %w", createErr)
		}
		builtinErrorFile = file.Name()
		if closeErr := file.Close(); closeErr != nil {
			_ = os.Remove(builtinErrorFile)
			return apiv1.ResultEnvelope{}, fmt.Errorf("executor: close built-in error file: %w", closeErr)
		}
		defer func() { _ = os.Remove(builtinErrorFile) }()
		stageEnv = append(stageEnv, BuiltinErrorFileEnvVar+"="+builtinErrorFile)
	}
	telemetryDir := telemetry.PrepareStageTelemetryDir(env.Workspace)
	if telemetryDir != "" {
		stageEnv = append(stageEnv, telemetry.StageTelemetryEnv+"="+telemetryDir)
	}

	// The tmp:ephemeral binding for runner `self`. It is applied LAST, over the
	// fully assembled environment, because it is an effect on the environment
	// rather than another contributor to it: whatever TMPDIR or temp-nested
	// cache the allowlist, the instance passthrough, or the stage's own
	// run.env produced, the attempt-private area is what the stage actually
	// gets. The directory is reclaimed by the deferred Reclaim below on every
	// exit path — success, stage failure, timeout, and the early returns
	// between here and the exec.
	//
	// It deliberately lives OUTSIDE env.Workspace. The workspace is the run's
	// continuity — the worktree whose delta later stages consume and whose
	// commits the run publishes — and a build cache materializing inside it
	// would surface as untracked worktree content. Temp goes to the temp root;
	// the workspace is not touched by this binding at all.
	if e.EphemeralTmp {
		scope, scopeErr := ephemeraltmp.Establish(e.EphemeralTmpRoot)
		if scopeErr != nil {
			return apiv1.ResultEnvelope{}, fmt.Errorf("executor: bind tmp:ephemeral for stage %q: %w", env.TaskID, scopeErr)
		}
		defer func() { _ = scope.Reclaim() }()
		stageEnv, err = scope.Apply(stageEnv)
		if err != nil {
			return apiv1.ResultEnvelope{}, fmt.Errorf("executor: bind tmp:ephemeral for stage %q: %w", env.TaskID, err)
		}
	}

	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// Substitute the running daemon's own binary for a bare "goobers" token: the
	// stage's cwd is a fresh worktree clone that never contains the goobers
	// binary, and a bare name would PATH-resolve against the daemon's PATH, not
	// the worktree — so it fails at exec (#229). SelfBin is byte-identical to the
	// running daemon, avoiding version skew.
	name := command[0]
	if e.SelfBin != "" && StageInvokesGoobersCLI(command) {
		name = e.SelfBin
	}
	invokeName, invokeArgs := commandInvocation(name, command[1:])
	cmd := exec.Command(invokeName, invokeArgs...)
	cmd.Dir = env.Workspace
	cmd.Env = stageEnv
	// Configure tree ownership before the network isolation below layers its
	// own SysProcAttr fields on: on unix proc puts the stage in a NEW SESSION
	// (Setsid, not Setpgid) with no controlling terminal, so a stage that
	// touches terminal state can't be STOPPED by job control (the "local-ci
	// hang", #846) and the whole tree can be killed as a unit on timeout. See
	// internal/platform/proc for the full rationale.
	proc.Configure(cmd)
	networkIsolationMarker, err := configureCommandNetwork(cmd, run.Network)
	if err != nil {
		return apiv1.ResultEnvelope{}, err
	}

	progress := func() { invoke.ReportProgress(runCtx) }
	stdout := &capturingWriter{limit: maxOutput, progress: progress}
	stderr := &capturingWriter{limit: maxOutput, progress: progress}
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	tree, err := proc.Start(cmd)
	if err != nil {
		return apiv1.ResultEnvelope{
			Status:  apiv1.ResultFailure,
			Error:   &apiv1.ErrorInfo{Code: "exec_start", Message: err.Error(), Retryable: false},
			Summary: fmt.Sprintf("failed to start %q", command[0]),
		}, nil
	}

	// --diagnostics watchdog: periodically snapshot a long-running stage
	// (native sample + process tree + lsof) into a buffer recorded as an
	// artifact below. Off (and free) unless Diagnostics is set.
	var diag diagBuffer
	var diagStop, diagDone chan struct{}
	if e.Diagnostics {
		diagStop = make(chan struct{})
		diagDone = make(chan struct{})
		// filepath.Base(command[0]) (not name, which may be SelfBin's absolute
		// path substituted in for a goobers-CLI stage above) so the diagnostic
		// keyword matches the operator-declared command a hung stage actually
		// runs, e.g. "npm"/"dotnet"/"pytest"/"mvn" (#2172).
		stageCmd := filepath.Base(command[0])
		go func() {
			defer close(diagDone)
			watchStageDiagnostics(cmd.Process.Pid, stageCmd, &diag, diagStop)
		}()
	}

	waitDone := make(chan error, 1)
	go func() { waitDone <- cmd.Wait() }()

	var timedOut, canceled bool
	var waitErr error
	select {
	case waitErr = <-waitDone:
	case <-runCtx.Done():
		// runCtx.Done() fires both when its own timeout elapses and when the
		// caller's ctx is canceled out from under it — distinguishing the two
		// via context.Cause matters even though only the timeout path is
		// reachable today (internal/runner's dispatch always uses
		// context.WithoutCancel): a future hard-shutdown path that DOES
		// cancel ctx must not be mislabeled as a retryable timeout (#122).
		if errors.Is(context.Cause(runCtx), context.DeadlineExceeded) {
			timedOut = true
		} else {
			canceled = true
		}
		// On a TIMEOUT, first SIGQUIT the whole process group so every Go
		// process in it dumps its full goroutine trace to the captured
		// stdout/stderr before dying — a stage that blew its timeout is exactly
		// the case worth diagnosing, and SIGKILL alone leaves no trace of WHY
		// it hung (the long-standing "killed at 10m, cmd/goobers never finished,
		// no dump" record). The final SIGKILL sweep always runs: the direct
		// child exiting after SIGQUIT does not prove that signal-ignoring
		// descendants exited too. A deliberate cancel (not a timeout) goes
		// straight to SIGKILL — nothing to diagnose there.
		waited := false
		if timedOut {
			// SIGQUIT the whole tree so every Go process in it dumps its full
			// goroutine trace and exits before the force-kill below. A platform
			// that can't signal tree members (windows Job Objects) reports the
			// request unsupported, and we fall straight through to Kill.
			if supported, _ := tree.RequestDump(); supported {
				select {
				case waitErr = <-waitDone:
					waited = true // goroutine traces are now in the captured output
				case <-time.After(timeoutDumpGrace):
				}
			}
		}
		// Kill the whole tree, not just the direct child, so a runaway
		// subprocess tree can't outlive the stage.
		_ = tree.Kill()
		if !waited {
			select {
			case waitErr = <-waitDone:
			case <-time.After(groupKillWaitDelay):
				// A descendant escaped the process group (e.g. via setsid) and
				// is still holding a stdout/stderr pipe open, so cmd.Wait()
				// never returns (#119) — give up waiting rather than hang the
				// stage (and graceful drain) forever. waitErr stays nil here,
				// but it's only read below in the non-timeout/non-canceled path,
				// so this bound never masks a real exit code.
			}
		}
	}

	if diagStop != nil {
		// Signal the watchdog to stop and wait for it to fully exit before
		// reading diag (below) — a clean join, so it can never touch diag or the
		// package-level timings concurrently after Run returns. Bounded in
		// practice: the watchdog is either idle between samples (returns at once)
		// or mid-`sample` (returns within its ~3s duration).
		close(diagStop)
		<-diagDone
	}

	outBytes := scrubber.Scrub(stdout.Bytes())
	errBytes := scrubber.Scrub(stderr.Bytes())

	result := apiv1.ResultEnvelope{Outputs: map[string]interface{}{}, Metrics: map[string]float64{}}
	if networkIsolationMarker != "" {
		// #2034: a non-empty marker means this network:none stage did NOT
		// actually run isolated (the Windows escape hatch fired) — visible
		// here in the journaled stage.finished Outputs (and from there the
		// portal's run/stage inspector), not only in the child process's own
		// GOOBERS_NETWORK_ISOLATION env var, so a host-global opt-out can't
		// silently de-isolate every later "isolated" stage with nothing in
		// the run record to show for it.
		result.Outputs["networkIsolation"] = networkIsolationMarker
	}

	// --diagnostics: record whatever the watchdog sampled from a long-running
	// stage. Best-effort — a record failure here must never fail the stage.
	if snap := diag.Bytes(); len(snap) > 0 {
		if ref, aerr := e.Journal.RecordArtifact(env.TaskID+"/diagnostics/stage-samples.txt", scrubber.Scrub(snap)); aerr == nil {
			result.Artifacts = append(result.Artifacts, refToPointer(ref, "text/plain"))
		}
	}

	stdoutRef, err := e.Journal.RecordArtifact(env.TaskID+"/stdout.log", outBytes)
	if err != nil {
		return apiv1.ResultEnvelope{}, fmt.Errorf("executor: record stdout: %w", err)
	}
	result.Artifacts = append(result.Artifacts, refToPointer(stdoutRef, "text/plain"))
	if stdout.Truncated() {
		result.Outputs["stdoutTruncated"] = true
	}

	stderrRef, err := e.Journal.RecordArtifact(env.TaskID+"/stderr.log", errBytes)
	if err != nil {
		return apiv1.ResultEnvelope{}, fmt.Errorf("executor: record stderr: %w", err)
	}
	result.Artifacts = append(result.Artifacts, refToPointer(stderrRef, "text/plain"))
	if stderr.Truncated() {
		result.Outputs["stderrTruncated"] = true
	}

	if timedOut {
		if StageInvokesProviderBuiltin(command) {
			return apiv1.ResultEnvelope{}, invoke.InfrastructureFailure(StageFailure("timeout", fmt.Errorf(
				"executor: provider stage %q exceeded timeout %s: %w",
				command[1], timeout, context.DeadlineExceeded,
			)))
		}
		result.Status = apiv1.ResultFailure
		result.Error = &apiv1.ErrorInfo{
			Code:      "timeout",
			Message:   fmt.Sprintf("stage exceeded timeout %s", timeout),
			Retryable: true,
		}
		result.Summary = "stage timed out and was killed"
		return result, nil
	}
	if canceled {
		// Distinct from "timeout": the stage's own deadline had not elapsed —
		// its context was canceled for some other reason (unreachable today,
		// see the select above's doc comment). Not retryable: unlike a
		// transient timeout, a deliberate cancellation should not be retried
		// the same way.
		result.Status = apiv1.ResultFailure
		result.Error = &apiv1.ErrorInfo{
			Code:      "canceled",
			Message:   "stage's context was canceled (not a timeout)",
			Retryable: false,
		}
		result.Summary = "stage was canceled"
		return result, nil
	}

	exitCode := exitCodeOf(waitErr)
	result.Metrics["exitCode"] = float64(exitCode)

	if builtinErrorFile != "" {
		report, reportErr := readBuiltinErrorReport(builtinErrorFile)
		if reportErr != nil {
			return apiv1.ResultEnvelope{}, fmt.Errorf("executor: read built-in error report: %w", reportErr)
		}
		if report != nil {
			message := report.Message
			if message == "" {
				message = fmt.Sprintf("command exited %d", exitCode)
			}
			stageName := command[0]
			if len(command) > 1 {
				stageName = command[1]
			}
			if report.Retryable {
				return apiv1.ResultEnvelope{}, invoke.InfrastructureFailure(StageFailure(report.Code, fmt.Errorf(
					"executor: goobers stage %q reported %s: %s", stageName, report.Code, message,
				)))
			}
			result.Status = apiv1.ResultFailure
			result.Error = &apiv1.ErrorInfo{Code: report.Code, Message: message, Retryable: false}
			result.Summary = message
			return result, nil
		}
	}

	if exitCode != 0 && StageInvokesProviderBuiltin(command) {
		// #control precedence ruling (2026-07-17, the #613/#711/#712
		// chokepoint): a provider-builtin stage that got far enough to
		// self-report structurally via its declared result file
		// (failProviderStage's OutputErrorCode, #614) is a richer, more
		// specific signal than raw stderr text — use it, and skip stderr
		// classification entirely, so #711's fine-grained codes and #712's
		// result.Outputs["rateLimitReset"] read stay authoritative instead
		// of being silently reclassified by this intercept. Only fall
		// through to stderr-text classification below when no structured
		// result exists at all — the residual case this intercept actually
		// exists for: a stage that died before it could call
		// failProviderStage (bad flags, signal kill, panic).
		if resultFile != "" {
			if full, perr := apiv1.ResolveContainedPath(env.Workspace, resultFile); perr == nil {
				if data, rerr := os.ReadFile(full); rerr == nil {
					ref, aerr := e.recordResultArtifact(
						env.TaskID+"/result", scrubber.Scrub(data), StageInvokesProviderBuiltin(command),
					)
					if aerr != nil {
						return apiv1.ResultEnvelope{}, fmt.Errorf("executor: record result file: %w", aerr)
					}
					result.Artifacts = append(result.Artifacts, refToPointer(ref, mediaTypeFor(resultFile)))
					mergeResultFileOutputs(&result, data)
					code, message, retryable := consumeErrorOutputs(result.Outputs)
					if code != "" {
						if message == "" {
							message = fmt.Sprintf("command exited %d", exitCode)
						}
						if retryable {
							return apiv1.ResultEnvelope{}, providerStageInfrastructureFailure(command[1], code, message, result.Outputs)
						}

						result.Status = apiv1.ResultFailure
						result.Error = &apiv1.ErrorInfo{Code: code, Message: message, Retryable: false}
						result.Summary = message
						return result, nil
					}

					// The file existed and parsed but carried no
					// OutputErrorCode (the stage self-reported success
					// shape yet still exited nonzero, or wrote an
					// unrelated result) — its artifact/outputs stay
					// attached to result either way; fall through to
					// stderr classification below for the actual verdict.
				}
				// A read error (including not-yet-written, the common
				// crashed-before-writing case) is not fatal here — falls
				// through to stderr classification, exactly as before this
				// check existed.
			}
		}
		message := lastNonEmptyLine(errBytes)
		if message == "" {
			message = fmt.Sprintf("command exited %d", exitCode)
		}
		providerErr := errors.New(message)
		if providers.IsTransientError(providerErr) {
			return apiv1.ResultEnvelope{}, invoke.InfrastructureFailure(StageFailure("provider_error", fmt.Errorf(
				"executor: provider stage %q failed: %w", command[1], providerErr,
			)))
		}
		result.Status = apiv1.ResultFailure
		result.Error = &apiv1.ErrorInfo{
			Code:      "provider_error",
			Message:   providerErr.Error(),
			Retryable: false,
		}
		result.Summary = fmt.Sprintf("provider stage %q failed", command[1])
		return result, nil
	}

	if resultFile != "" {
		full, perr := apiv1.ResolveContainedPath(env.Workspace, resultFile)
		switch {
		case perr == nil:
			data, rerr := os.ReadFile(full)
			switch {
			case rerr == nil:
				ref, aerr := e.recordResultArtifact(
					env.TaskID+"/result", scrubber.Scrub(data), StageInvokesProviderBuiltin(command),
				)
				if aerr != nil {
					return apiv1.ResultEnvelope{}, fmt.Errorf("executor: record result file: %w", aerr)
				}
				result.Artifacts = append(result.Artifacts, refToPointer(ref, mediaTypeFor(resultFile)))
				mergeResultFileOutputs(&result, data)
			case os.IsNotExist(rerr):
				result.Status = apiv1.ResultFailure
				result.Error = missingResultFileError(resultFile, exitCode, waitErr, errBytes)
				result.Summary = "declared result file missing"
				return result, nil
			default:
				// rerr here is an *fs.PathError (or similar) wrapping the
				// underlying syscall.Errno — %w already carries that errno
				// text (e.g. "permission denied") into this executor-level
				// error, which internal/runner's runTask journals verbatim
				// as an executor_error event (#711): no separate logging
				// needed, the errno reaches the run journal through the
				// normal error-propagation path.
				return apiv1.ResultEnvelope{}, fmt.Errorf("executor: read result file %q: %w", resultFile, rerr)
			}
		case errors.Is(perr, os.ErrNotExist):
			// A missing component in the declared path resolves the same way
			// EvalSymlinks reports a plain missing file — same UX as above.
			result.Status = apiv1.ResultFailure
			result.Error = missingResultFileError(resultFile, exitCode, waitErr, errBytes)
			result.Summary = "declared result file missing"
			return result, nil
		case errors.Is(perr, apiv1.ErrPathEscape), errors.Is(perr, apiv1.ErrSymlinkEscape):
			// Untrusted declared path (#120): escapes the workspace lexically
			// or via a symlink. Fail the stage closed, never follow it.
			result.Status = apiv1.ResultFailure
			result.Error = &apiv1.ErrorInfo{
				Code:      "result_file_path_escape",
				Message:   fmt.Sprintf("declared result file %q escapes the workspace: %v", resultFile, perr),
				Retryable: false,
			}
			result.Summary = "declared result file path escapes the workspace"
			return result, nil
		default:
			return apiv1.ResultEnvelope{}, fmt.Errorf("executor: resolve result file %q: %w", resultFile, perr)
		}
	}

	code, message, retryable := consumeErrorOutputs(result.Outputs)
	if exitCode == 0 {
		// OutputNoWork (issue #233) only ever downgrades a would-be Success
		// to NoWork — it's read from result.Outputs, which is only ever
		// populated by a successful declared-result-file read above, never
		// on a failure path (those all return early). A stage with no
		// declared resultFile has result.Outputs empty here, so this is a
		// no-op for it.
		if v, ok := result.Outputs[OutputNoWork].(bool); ok && v {
			result.Status = apiv1.ResultNoWork
			result.Summary = "stage found no work to do"
			return result, nil
		}
		result.Status = apiv1.ResultSuccess
		result.Summary = "stage completed"
		return result, nil
	}
	result.Status = apiv1.ResultFailure
	// A typed error reported through the declared result file (see
	// OutputErrorCode) beats the generic nonzero_exit: the command knew
	// exactly why it failed and said so structurally.
	if code != "" {
		if message == "" {
			message = fmt.Sprintf("command exited %d", exitCode)
		}
		result.Error = &apiv1.ErrorInfo{Code: code, Message: message, Retryable: retryable}
		result.Summary = message
		return result, nil
	}
	result.Error = &apiv1.ErrorInfo{
		Code:      "nonzero_exit",
		Message:   fmt.Sprintf("command exited %d", exitCode),
		Retryable: false,
	}
	diagnostic := summarizeCommandFailure(outBytes, errBytes)
	if applyCommandFailureDiagnostic(&result, exitCode, diagnostic, stdoutRef.Path, stderrRef.Path) {
		return result, nil
	}
	if excerpt := stderrFailureExcerpt(errBytes); excerpt != "" {
		result.Error.Message += "; stderr: " + excerpt
	}
	result.Summary = fmt.Sprintf("command exited %d", exitCode)
	return result, nil
}

func consumeErrorOutputs(outputs map[string]interface{}) (code, message string, retryable bool) {
	code, _ = outputs[OutputErrorCode].(string)
	message, _ = outputs[OutputErrorMessage].(string)
	retryable, _ = outputs[OutputErrorRetryable].(bool)
	delete(outputs, OutputErrorCode)
	delete(outputs, OutputErrorMessage)
	delete(outputs, OutputErrorRetryable)
	return code, message, retryable
}

func providerStageInfrastructureFailure(stage, code, message string, outputs map[string]interface{}) error {
	err := StageFailure(code, fmt.Errorf("executor: provider stage %q reported %s: %s", stage, code, message))
	if code != providers.ErrorCodeRateLimited {
		return invoke.InfrastructureFailure(err)
	}
	resetValue, _ := outputs["rateLimitReset"].(string)
	resetAt, parseErr := time.Parse(time.RFC3339, resetValue)
	if parseErr != nil {
		return invoke.InfrastructureFailure(err)
	}
	return invoke.InfrastructureFailureUntil(err, resetAt.Add(providerRateLimitResetSlack))
}

func readBuiltinErrorReport(path string) (*builtinErrorReport, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, nil
	}
	var report builtinErrorReport
	if err := json.Unmarshal(data, &report); err != nil {
		return nil, fmt.Errorf("decode %s: %w", path, err)
	}
	if report.Code == "" {
		return nil, fmt.Errorf("decode %s: errorCode is required", path)
	}
	return &report, nil
}

func lastNonEmptyLine(data []byte) string {
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if line := strings.TrimSpace(lines[i]); line != "" {
			return line
		}
	}
	return ""
}

func (e *ShellExecutor) timeoutFor(env apiv1.InvocationEnvelope) (time.Duration, error) {
	if env.Limits.MaxDurationSeconds > 0 {
		return time.Duration(env.Limits.MaxDurationSeconds) * time.Second, nil
	}
	if s := stringInput(env, InputTimeout); s != "" {
		d, err := time.ParseDuration(s)
		if err != nil {
			return 0, fmt.Errorf("executor: invalid %s input %q: %w", InputTimeout, s, err)
		}
		return d, nil
	}
	if e.DefaultTimeout > 0 {
		return e.DefaultTimeout, nil
	}
	return DefaultTimeout, nil
}

func (e *ShellExecutor) maxOutputFor(env apiv1.InvocationEnvelope) (int64, error) {
	if s := stringInput(env, InputMaxOutputBytes); s != "" {
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil || n <= 0 {
			return 0, fmt.Errorf("executor: invalid %s input %q", InputMaxOutputBytes, s)
		}
		return n, nil
	}
	if e.DefaultMaxOutputBytes > 0 {
		return e.DefaultMaxOutputBytes, nil
	}
	return DefaultMaxOutputBytes, nil
}

func stringInput(env apiv1.InvocationEnvelope, key string) string {
	v, ok := env.Inputs[key]
	if !ok {
		return ""
	}
	s, _ := v.(string)
	return s
}

// mergeResultFileOutputs best-effort-parses a declared result file's bytes as
// a flat JSON object and merges its string/number/bool fields into
// result.Outputs — see InputResultFile's doc comment. data that isn't JSON,
// or isn't a flat object, is silently left alone: the artifact/presence-check
// contract InputResultFile already provides holds either way, and not every
// declared result file is meant to carry structured outputs.
func mergeResultFileOutputs(result *apiv1.ResultEnvelope, data []byte) {
	var m map[string]interface{}
	if err := json.Unmarshal(data, &m); err != nil {
		return
	}
	for k, v := range m {
		switch v.(type) {
		case string, float64, bool:
			result.Outputs[k] = v
		}
	}
}

func exitCodeOf(err error) int {
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	return -1
}

// missingResultFileStderrExcerptBytes bounds the stderr excerpt
// missingResultFileError attaches (#711) — enough to show the actual cause
// (a stack trace's top frame, a "command not found", a panic message)
// without ballooning the journaled ErrorInfo.Message.
const missingResultFileStderrExcerptBytes = 512

// missingResultFileError builds the diagnostic ErrorInfo for a declared
// result file that was never produced (#711). The bare "was not produced"
// message gave an operator nothing to work with — a command that exited 0
// but forgot to write its file, one that was SIGKILLed mid-run, and one
// whose own logic failed before it ever reached the result-file step all
// looked identical. This distinguishes them: exitCode (Go's exec.ExitError
// convention: -1 when the process died to a signal, not a normal exit) is
// replaced with the actual signal name when the process was signaled
// (signalOf), and a bounded stderr excerpt is appended when the process
// produced any.
func missingResultFileError(resultFile string, exitCode int, waitErr error, errBytes []byte) *apiv1.ErrorInfo {
	detail := fmt.Sprintf("exit code %d", exitCode)
	if sig, ok := signalOf(waitErr); ok {
		detail = fmt.Sprintf("killed by signal %s", sig)
	}
	msg := fmt.Sprintf("declared result file %q was not produced (%s)", resultFile, detail)
	if excerpt := stderrExcerpt(errBytes); excerpt != "" {
		msg += "; stderr: " + excerpt
	}
	return &apiv1.ErrorInfo{Code: "missing_result_file", Message: msg, Retryable: false}
}

// signalOf reports the signal that terminated the process behind waitErr, if
// it died to one (as opposed to a normal, possibly nonzero, exit) — the
// distinction exitCodeOf's -1 sentinel alone loses (a signal death and an
// exec.ExitError of some other unexpected shape both report -1).
func signalOf(waitErr error) (syscall.Signal, bool) {
	var exitErr *exec.ExitError
	if !errors.As(waitErr, &exitErr) {
		return 0, false
	}
	ws, ok := exitErr.Sys().(syscall.WaitStatus)
	if !ok || !ws.Signaled() {
		return 0, false
	}
	return ws.Signal(), true
}

// stderrExcerpt returns a bounded, trimmed, "…"-suffixed-when-truncated
// prefix of errBytes (already secret-scrubbed by Run's caller) for
// missingResultFileError. Empty input yields "" so the caller can skip
// appending an empty "; stderr: " clause.
func stderrExcerpt(errBytes []byte) string {
	if len(errBytes) == 0 {
		return ""
	}
	b := errBytes
	truncated := false
	if len(b) > missingResultFileStderrExcerptBytes {
		b = b[:missingResultFileStderrExcerptBytes]
		truncated = true
	}
	s := strings.TrimSpace(string(b))
	if truncated {
		s += "…"
	}
	return s
}

// Generic command failures keep both ends: runtimes may print the cause before
// a long stack dump, while tools conventionally print terminal causes last.
func stderrFailureExcerpt(errBytes []byte) string {
	if len(errBytes) == 0 {
		return ""
	}
	if len(errBytes) <= missingResultFileStderrExcerptBytes {
		return strings.TrimSpace(string(errBytes))
	}
	headBytes := missingResultFileStderrExcerptBytes / 2
	tailBytes := missingResultFileStderrExcerptBytes - headBytes
	head := strings.TrimSpace(string(errBytes[:headBytes]))
	tail := strings.TrimSpace(string(errBytes[len(errBytes)-tailBytes:]))
	return head + "…" + tail
}

func refToPointer(ref journal.Ref, mediaType string) apiv1.ArtifactPointer {
	return apiv1.ArtifactPointer{
		Path: ref.Path, Digest: ref.Digest, MediaType: mediaType, Size: ref.Size, Integrity: ref.Integrity,
	}
}

func (e *ShellExecutor) recordResultArtifact(name string, data []byte, preserveProviderIntegrity bool) (journal.Ref, error) {
	if !preserveProviderIntegrity {
		return e.Journal.RecordArtifact(name, data)
	}
	integrity, err := providerResultIntegrity(data)
	if err != nil {
		return journal.Ref{}, fmt.Errorf("provider result integrity: %w", err)
	}
	recorder, ok := e.Journal.(integrityArtifactRecorder)
	if !ok {
		return journal.Ref{}, errors.New("provider result integrity recorder is unavailable")
	}
	return recorder.RecordArtifactWithIntegrity(name, data, integrity)
}

func providerResultIntegrity(data []byte) (apiv1.Integrity, error) {
	var result interface{}
	if err := json.Unmarshal(data, &result); err != nil {
		return "", fmt.Errorf("decode JSON: %w", err)
	}
	var grades []apiv1.Integrity
	var walk func(interface{}) error
	walk = func(value interface{}) error {
		switch value := value.(type) {
		case map[string]interface{}:
			for key, child := range value {
				if key == "integrity" {
					raw, ok := child.(string)
					grade := apiv1.Integrity(raw)
					if !ok || !grade.Valid() {
						return fmt.Errorf("invalid integrity label %v", child)
					}
					grades = append(grades, grade)
					continue
				}
				if err := walk(child); err != nil {
					return err
				}
			}
		case []interface{}:
			for _, child := range value {
				if err := walk(child); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := walk(result); err != nil {
		return "", err
	}
	integrity := apiv1.WeakestIntegrity(grades...)
	if !integrity.Valid() {
		return "", errors.New("no valid integrity label")
	}
	return integrity, nil
}

func mediaTypeFor(path string) string {
	if strings.HasSuffix(path, ".json") {
		return "application/json"
	}
	return "application/octet-stream"
}

// capturingWriter caps total bytes retained from a stream at limit, silently
// discarding (but still acknowledging, so the writer never blocks or errors
// the producing process) anything beyond it.
//
// Write is mutex-guarded because on a give-up timeout (#119's
// groupKillWaitDelay) Run stops waiting on cmd.Wait() while os/exec's own
// stdout/stderr-copying goroutines may still be running (an escaped
// descendant can hold a pipe open indefinitely) — Bytes must not read the
// buffer while such a goroutine could still be writing to it.
type capturingWriter struct {
	mu        sync.Mutex
	buf       bytes.Buffer
	limit     int64
	truncated bool
	progress  func()
}

func (w *capturingWriter) Write(p []byte) (int, error) {
	if len(p) > 0 && w.progress != nil {
		w.progress()
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.truncated {
		return len(p), nil
	}
	remaining := w.limit - int64(w.buf.Len())
	if remaining <= 0 {
		w.truncated = true
		return len(p), nil
	}
	if int64(len(p)) > remaining {
		w.buf.Write(p[:remaining])
		w.truncated = true
		return len(p), nil
	}
	w.buf.Write(p)
	return len(p), nil
}

// Bytes returns a snapshot of what's been captured so far. Safe to call
// concurrently with Write (see the type doc for why that matters here).
func (w *capturingWriter) Bytes() []byte {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]byte(nil), w.buf.Bytes()...)
}

// Truncated reports whether the cap has been hit. Safe to call concurrently
// with Write, for the same reason as Bytes.
func (w *capturingWriter) Truncated() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.truncated
}

// --- --diagnostics stage watchdog -------------------------------------------

// diagnosticsSampleAfter is how long a stage must run before the --diagnostics
// watchdog takes its first snapshot. Comfortably above a healthy local-ci
// `make ci` (~1-2 min) so a normal stage is never sampled, but well below the
// default 10m stage timeout so a hung stage is captured several times before it
// is killed.
// vars (not consts) so tests can shrink them; production never mutates them.
var diagnosticsSampleAfter = 2 * time.Minute

// diagnosticsSampleInterval / diagnosticsMaxSamples bound the watchdog: a few
// snapshots spaced out, all landing before the stage timeout so the watchdog is
// never mid-`sample` (which briefly SIGSTOPs the target) when the timeout path
// signals it — 2m + 3×2m = 8m < 10m.
var diagnosticsSampleInterval = 2 * time.Minute
var diagnosticsMaxSamples = 3

// diagnosticsCapture snapshots a still-running stage subprocess for the
// --diagnostics watchdog: its process tree, its open fds (lsof — reveals the
// pipe/self-pipe fds behind an I/O deadlock), and a native thread `sample`
// (macOS) — the OS-level stacks that show a wedged `go test -race` stage even
// when the Go runtime can't stopTheWorld to dump goroutines. A var so tests can
// stub it; the default is best-effort and skips any tool that isn't present.
// The second argument is the stage's command basename (argv[0]) — see
// watchStageDiagnostics.
var diagnosticsCapture = defaultDiagnosticsCapture

// watchStageDiagnostics takes up to diagnosticsMaxSamples snapshots of a
// long-running stage into dst, starting after diagnosticsSampleAfter. It stops
// immediately when stop is closed (the stage finished or was killed). stageCmd
// is the stage's compiled command basename (argv[0]), threaded through so the
// unix capture's process-tree keyword filter always matches the actual hung
// process regardless of stack (#2172).
func watchStageDiagnostics(pid int, stageCmd string, dst *diagBuffer, stop <-chan struct{}) {
	select {
	case <-stop:
		return
	case <-time.After(diagnosticsSampleAfter):
	}
	for n := 1; n <= diagnosticsMaxSamples; n++ {
		if snap := diagnosticsCapture(pid, stageCmd); len(snap) > 0 {
			dst.WriteSnapshot(n, snap)
		}
		select {
		case <-stop:
			return
		case <-time.After(diagnosticsSampleInterval):
		}
	}
}

// diagBuffer is a concurrency-safe sink for watchStageDiagnostics: the watchdog
// goroutine appends snapshots while Run proceeds, and Run reads the whole thing
// once the stage is done to record it as an artifact.
type diagBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (d *diagBuffer) WriteSnapshot(n int, snap []byte) {
	d.mu.Lock()
	defer d.mu.Unlock()
	fmt.Fprintf(&d.buf, "\n========== diagnostics sample #%d ==========\n", n)
	d.buf.Write(snap)
}

func (d *diagBuffer) Bytes() []byte {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]byte(nil), d.buf.Bytes()...)
}
