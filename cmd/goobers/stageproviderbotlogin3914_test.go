package main

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/procenv"
	"github.com/goobers/goobers/internal/runnercap"
	"github.com/goobers/goobers/providers"
)

// stageproviderbotlogin3914_test.go is Goobers#3914's regression evidence.
//
// MEASURED failure: a stage running in a POD has no instance root, so
// stageProviderConfiguredLogin's fail-open instance.LoadConfig always returned
// "" there. providers.WithConfiguredLogin was therefore never applied and
// AuthenticatedLogin fell through to GET /user — an endpoint a GitHub App
// INSTALLATION token cannot call ("Resource not accessible by integration").
// Every trusted-comment check in a pod (claim markers, verdict authorship,
// handoff authorship, PR self-selection) either failed with a forge error for
// a platform fault or silently read as "not mine". #3885/#3890 fixed the
// LOCAL half of exactly this; #3910 then shrank the instance-root guard list
// from 22 stages to 7, so many more stages now reach a pod and hit it.
//
// The fix resolves the login DAEMON-SIDE, where the config is readable, and
// stamps it into the pod as dispatcher-owned run identity. These tests use the
// REAL constructors and the REAL dispatcher renderer end to end: a stubbed
// seam would have passed against the bug.

// podStageEnv materialises into this test process exactly the environment the
// dispatcher renders for a goobers-CLI stage pod routed to repo, wired from
// the instance config at root the way workerdispatch.go wires it.
//
// The env is RENDERED, never hand-written: hand-writing it would assert what
// the test author believed stageEnv emits, which is how #3725's allowlist gap
// reached review. The whole control plane is cleared first so a name the
// dispatcher does NOT stamp cannot arrive from the test process's own
// environment.
func podStageEnv(t *testing.T, root string, repo providers.RepositoryRef) {
	t.Helper()

	var botLogins map[string]string
	if root != "" {
		cfg, err := instance.LoadConfig(instance.NewLayout(root).ConfigFile())
		if err != nil {
			t.Fatalf("LoadConfig: %v", err)
		}
		botLogins = cfg.GitHubBotLogins()
	}

	pod, err := dispatcher.RenderPod(
		dispatcher.Config{Namespace: "gaggle-e2e", BotLogins: botLogins},
		dispatcher.Attempt{
			RunID:    "run-3914",
			Gaggle:   "e2e",
			Workflow: "implementation",
			Stage:    "apply-verdict",
			Number:   1,
			Timeout:  30 * time.Second,
			PodToken: "goobers-pod.tok",
			Command:  []string{"goobers", "apply-verdict"},
			CLIStage: true,
			RunContext: map[string]string{
				"GOOBERS_REPO_PROVIDER":  string(repo.Provider),
				executor.RepoOwnerEnvVar: repo.Owner,
				executor.RepoNameEnvVar:  repo.Name,
			},
		},
		dispatcher.RunnerSpec{
			Name:         "linux-cli",
			OS:           "linux",
			HostKind:     instance.RunnerHostImage,
			Host:         "ghcr.io/goobers/goobers-base:0123456789abcdef0123456789abcdef01234567",
			Restrictions: []string{string(runnercap.RestrictionEnvDefaultDeny)},
		},
	)
	if err != nil {
		t.Fatalf("RenderPod: %v", err)
	}

	for _, name := range dispatcher.DispatcherControlEnv {
		unsetForTest(t, name)
	}
	for _, e := range pod.Spec.Containers[0].Env {
		t.Setenv(e.Name, e.Value)
	}
	// A pod has no instance root: the dispatcher never stamps one, which is
	// the whole reason the login has to arrive some other way.
	unsetForTest(t, executor.InstanceRootEnvVar)
}

// unsetForTest removes a variable for the duration of the test and restores it
// afterwards. t.Setenv(name, "") would not do: the empty string is a
// MEANINGFUL stamp ("resolved: this repo declares no bot login") and the three
// states absent/empty/set are exactly what #3914 turns on, so a test that
// cannot express "absent" cannot test the fix.
func unsetForTest(t *testing.T, name string) {
	t.Helper()
	t.Setenv(name, "") // registers the restore-on-cleanup
	if err := os.Unsetenv(name); err != nil {
		t.Fatalf("unset %s: %v", name, err)
	}
}

// podStageRoot is the instance root a stage sees in a pod: none. The path
// exists (the stage has a workspace) but carries no instance config, which is
// what makes the local lookup unavailable there.
func podStageRoot(t *testing.T) string {
	t.Helper()
	return t.TempDir()
}

// resolveStageLogin builds a provider through a REAL stage constructor and
// asks it who it is, against a forge that records every request.
func resolveStageLogin(t *testing.T, root string, repo providers.RepositoryRef, forge *recordingForge) (string, error) {
	t.Helper()
	provider, err := newApplyVerdictProviderForRepo(root, repo)
	if err != nil {
		t.Fatalf("newApplyVerdictProviderForRepo: %v", err)
	}
	githubProvider, ok := provider.(*providers.GitHubProvider)
	if !ok {
		t.Fatalf("provider type = %T, want *providers.GitHubProvider", provider)
	}
	githubProvider.BaseURL = forge.server.URL
	return githubProvider.AuthenticatedLogin(context.Background())
}

// THE #3914 TEST. Under a GitHub App installation token, a stage running in a
// pod resolves the SAME login the same stage resolves locally, and makes ZERO
// API requests doing it.
//
// Both halves run against the same instance config, in one test, so they
// cannot drift: the pod half's identity is produced by dispatcher.RenderPod
// from that config, not asserted as a literal.
func TestPodStageResolvesTheSameLoginAsLocalModeWithoutUserRequest(t *testing.T) {
	root := initDemo(t)
	repo := declareGitHubAppAuth(t, root, "goobersbot")
	t.Setenv(executor.CredentialEnvVar(string(capability.ProviderPRWrite)), "ghs-installation-token")

	// LOCAL: the config is readable, and #3885's fix answers from it.
	localForge := newRecordingForge(t, "")
	unsetForTest(t, dispatcher.ProviderBotLoginEnv)
	localLogin, err := resolveStageLogin(t, root, repo, localForge)
	if err != nil {
		t.Fatalf("local AuthenticatedLogin: %v", err)
	}
	if localLogin != "goobersbot[bot]" {
		t.Fatalf("local AuthenticatedLogin = %q, want %q", localLogin, "goobersbot[bot]")
	}

	// POD: no readable config, identity arrives only as dispatcher-stamped
	// run identity.
	podStageEnv(t, root, repo)
	podForge := newRecordingForge(t, "")
	podLogin, err := resolveStageLogin(t, podStageRoot(t), repo, podForge)
	if err != nil {
		t.Fatalf("pod AuthenticatedLogin: %v", err)
	}
	if podLogin != localLogin {
		t.Fatalf("pod login %q != local login %q; a stage must not have a different identity depending on where it runs", podLogin, localLogin)
	}
	if paths := podForge.requestedPaths(); len(paths) != 0 {
		t.Fatalf("pod stage made %d API request(s) %v under App auth, want none — GET /user 403s for an installation token, which is #3914", len(paths), paths)
	}
}

// ABLATION. The same pod, the same App token, the same constructor — with the
// stamp REMOVED. If this passes with a login, the test above is not measuring
// the stamp; if it 403s its way to a forge error, the fix has silently
// regressed to the #3914 behaviour.
//
// It must instead fail CLOSED: no HTTP request at all, and a local, actionable
// error naming the missing identity.
func TestPodStageWithoutTheStampFailsClosedWithoutAskingTheForge(t *testing.T) {
	root := initDemo(t)
	repo := declareGitHubAppAuth(t, root, "goobersbot")
	t.Setenv(executor.CredentialEnvVar(string(capability.ProviderPRWrite)), "ghs-installation-token")

	podStageEnv(t, root, repo)
	unsetForTest(t, dispatcher.ProviderBotLoginEnv) // the ablation

	forge := newRecordingForge(t, "")
	_, err := resolveStageLogin(t, podStageRoot(t), repo, forge)

	var refused *providers.LoginSelfReportRefusedError
	if !errors.As(err, &refused) {
		t.Fatalf("AuthenticatedLogin err = %v, want *providers.LoginSelfReportRefusedError; falling back to GET /user in a pod IS Goobers#3914", err)
	}
	if !strings.Contains(refused.Reason, dispatcher.ProviderBotLoginEnv) {
		t.Fatalf("refusal %q does not name %s; an operator has to be able to act on it", refused.Reason, dispatcher.ProviderBotLoginEnv)
	}
	if paths := forge.requestedPaths(); len(paths) != 0 {
		t.Fatalf("refusal cost %d API request(s) %v, want none — it is decided locally, before any transport", len(paths), paths)
	}
}

// A stage that never consults AuthenticatedLogin must be UNAFFECTED by an
// unresolved login. The refusal travels into the provider; it is not a
// construction error, because "genuinely unconfigured provider" is a supported
// posture and turning it into a refused stage would break every stage that
// only reads and writes issues.
func TestPodStageWithoutTheStampStillConstructsAndOperates(t *testing.T) {
	root := initDemo(t)
	repo := declareGitHubAppAuth(t, root, "goobersbot")
	t.Setenv(executor.CredentialEnvVar(string(capability.ProviderPRWrite)), "ghs-installation-token")

	podStageEnv(t, root, repo)
	unsetForTest(t, dispatcher.ProviderBotLoginEnv)

	provider, err := newApplyVerdictProviderForRepo(podStageRoot(t), repo)
	if err != nil {
		t.Fatalf("newApplyVerdictProviderForRepo: %v — an unresolved login must not refuse CONSTRUCTION", err)
	}
	githubProvider, ok := provider.(*providers.GitHubProvider)
	if !ok {
		t.Fatalf("provider type = %T, want *providers.GitHubProvider", provider)
	}
	forge := newRecordingForge(t, "")
	githubProvider.BaseURL = forge.server.URL
	// Any ordinary API call still goes out: only identity self-report is refused.
	_, _ = githubProvider.GetWorkItem(context.Background(), repo, "1")
	if len(forge.requestedPaths()) == 0 {
		t.Fatal("provider made no request at all; the refusal must be scoped to AuthenticatedLogin, not to the whole provider")
	}
}

// PAT COMPATIBILITY, in a pod. The dispatcher stamps the variable for every
// CLI stage, EMPTY included, and empty means "resolved: this repo declares no
// bot login". That is the PAT posture and GET /user is the correct answer for
// it — the same answer local mode gives for the same repository, which is the
// parity #3914 asks for rather than merely the App case working.
func TestPodStageWithAPATStillSelfReportsThroughUser(t *testing.T) {
	root := initDemo(t) // demo config declares a PAT repo: no auth.slug
	cfg, err := instance.LoadConfig(instance.NewLayout(root).ConfigFile())
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	repo := githubRepoRefFromConfig(t, cfg)
	t.Setenv(executor.CredentialEnvVar(string(capability.ProviderPRWrite)), "pat-token")

	podStageEnv(t, root, repo)
	if value, stamped := os.LookupEnv(dispatcher.ProviderBotLoginEnv); !stamped || value != "" {
		t.Fatalf("%s = %q (stamped=%v); a PAT repo must be stamped PRESENT-and-EMPTY, which is what keeps absence meaning 'unresolved'",
			dispatcher.ProviderBotLoginEnv, value, stamped)
	}

	forge := newRecordingForge(t, "pat-user")
	login, err := resolveStageLogin(t, podStageRoot(t), repo, forge)
	if err != nil {
		t.Fatalf("AuthenticatedLogin: %v", err)
	}
	if login != "pat-user" {
		t.Fatalf("AuthenticatedLogin = %q, want %q", login, "pat-user")
	}
	if !forge.requested("/user") {
		t.Fatalf("requested paths = %v, want GET /user — a PAT self-reports and must keep doing so", forge.requestedPaths())
	}
}

// A MALFORMED stamp fails closed rather than falling back. A truncated value,
// a config blob, a value with a newline or a bare "[bot]" is an identity that
// matches no comment author, so every trusted-comment check would read as "not
// mine" — silently, and only in a pod. Refusing says so instead.
func TestPodStageMalformedStampFailsClosed(t *testing.T) {
	for _, stamped := range []string{
		"not a login",
		"goobers/bot",
		"goobersbot[bot]extra",
		"[bot]",
		"-leading-hyphen",
		strings.Repeat("a", 60),
		"kind: instance\nrepos:\n  - provider: github\n",
	} {
		t.Run(stamped, func(t *testing.T) {
			root := initDemo(t)
			repo := declareGitHubAppAuth(t, root, "goobersbot")
			t.Setenv(executor.CredentialEnvVar(string(capability.ProviderPRWrite)), "ghs-installation-token")

			podStageEnv(t, root, repo)
			t.Setenv(dispatcher.ProviderBotLoginEnv, stamped)

			forge := newRecordingForge(t, "")
			_, err := resolveStageLogin(t, podStageRoot(t), repo, forge)
			var refused *providers.LoginSelfReportRefusedError
			if !errors.As(err, &refused) {
				t.Fatalf("AuthenticatedLogin err = %v for stamp %q, want a refusal", err, stamped)
			}
			if paths := forge.requestedPaths(); len(paths) != 0 {
				t.Fatalf("malformed stamp still reached the forge: %v", paths)
			}
		})
	}
}

// The stamp is BOUND to the routed repository. It names exactly one repo — the
// one the run was routed to — so a provider built for some other repository
// (an additional repo, a cross-repo read) has no resolved identity and must
// not borrow the routed repo's login. In a pod there is no config to resolve
// one from, so this fails closed too.
func TestPodStageStampDoesNotTravelToAnotherRepository(t *testing.T) {
	root := initDemo(t)
	repo := declareGitHubAppAuth(t, root, "goobersbot")
	t.Setenv(executor.CredentialEnvVar(string(capability.ProviderPRWrite)), "ghs-installation-token")
	podStageEnv(t, root, repo)

	other := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: repo.Owner, Name: repo.Name + "-docs"}
	forge := newRecordingForge(t, "")
	_, err := resolveStageLogin(t, podStageRoot(t), other, forge)

	var refused *providers.LoginSelfReportRefusedError
	if !errors.As(err, &refused) {
		t.Fatalf("AuthenticatedLogin for %s/%s err = %v, want a refusal: the stamp describes %s/%s only",
			other.Owner, other.Name, err, repo.Owner, repo.Name)
	}
	if paths := forge.requestedPaths(); len(paths) != 0 {
		t.Fatalf("cross-repo refusal reached the forge: %v", paths)
	}
	// And the routed repository itself still resolves in the very same process.
	login, err := resolveStageLogin(t, podStageRoot(t), repo, newRecordingForge(t, ""))
	if err != nil || login != "goobersbot[bot]" {
		t.Fatalf("routed repo login = %q, err = %v; want %q", login, err, "goobersbot[bot]")
	}
}

// PRECEDENCE. The dispatcher-stamped identity wins over any config file the
// process can read. In a pod the only reachable config is one inside the
// WORKSPACE — content the run itself may have authored — so a stamp that
// yielded to a file on disk would hand a workflow the bot identity it is
// specifically not allowed to author. This is the $(VAR)/Env spoof's last
// remaining route, closed at the reader.
func TestWorkspaceConfigCannotOverrideTheStampedIdentity(t *testing.T) {
	root := initDemo(t)
	repo := declareGitHubAppAuth(t, root, "goobersbot")
	t.Setenv(executor.CredentialEnvVar(string(capability.ProviderPRWrite)), "ghs-installation-token")
	podStageEnv(t, root, repo)

	// A workspace the run wrote, declaring itself a different bot.
	hostile := initDemo(t)
	declareGitHubAppAuth(t, hostile, "attackerbot")

	login, err := resolveStageLogin(t, hostile, repo, newRecordingForge(t, ""))
	if err != nil {
		t.Fatalf("AuthenticatedLogin: %v", err)
	}
	if login != "goobersbot[bot]" {
		t.Fatalf("AuthenticatedLogin = %q, want the DISPATCHER's %q; a workflow-authored config must not choose the run's identity", login, "goobersbot[bot]")
	}
}

// LOCAL MODE IS UNCHANGED. The variable is never stamped locally — the
// executor does not set it and procenv's default-deny allowlist does not carry
// the name — so the local substrate takes the pre-existing config lookup
// exactly, including the PAT posture.
func TestLocalModeIdentityIsUnchanged(t *testing.T) {
	for _, name := range procenvBaseEnvNames(t) {
		if name == dispatcher.ProviderBotLoginEnv {
			t.Fatalf("%s is inherited into a local stage's environment; a local run could then be handed an identity from the host environment", name)
		}
	}

	root := initDemo(t)
	repo := declareGitHubAppAuth(t, root, "goobersbot")
	t.Setenv(executor.CredentialEnvVar(string(capability.ProviderPRWrite)), "ghs-installation-token")
	unsetForTest(t, dispatcher.ProviderBotLoginEnv)
	t.Setenv(executor.InstanceRootEnvVar, root)

	login, err := resolveStageLogin(t, root, repo, newRecordingForge(t, ""))
	if err != nil {
		t.Fatalf("AuthenticatedLogin: %v", err)
	}
	if login != "goobersbot[bot]" {
		t.Fatalf("local AuthenticatedLogin = %q, want %q", login, "goobersbot[bot]")
	}
}

// A local invocation whose config genuinely does not load keeps its
// pre-existing best-effort posture — GET /user — because an instance root IS
// declared, so this is not a pod. Fail-closed applies to the pod signal only;
// applying it here would refuse every standalone CLI invocation run outside an
// instance directory.
func TestLocalModeWithUnreadableConfigStillSelfReports(t *testing.T) {
	repo := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "web"}
	t.Setenv(executor.CredentialEnvVar(string(capability.ProviderPRWrite)), "pat-token")
	unsetForTest(t, dispatcher.ProviderBotLoginEnv)
	t.Setenv(executor.InstanceRootEnvVar, t.TempDir())

	forge := newRecordingForge(t, "pat-user")
	login, err := resolveStageLogin(t, t.TempDir(), repo, forge)
	if err != nil {
		t.Fatalf("AuthenticatedLogin: %v", err)
	}
	if login != "pat-user" || !forge.requested("/user") {
		t.Fatalf("login = %q, paths = %v; a local invocation with a declared root keeps the /user posture", login, forge.requestedPaths())
	}
}

func procenvBaseEnvNames(t *testing.T) []string {
	t.Helper()
	t.Setenv(dispatcher.ProviderBotLoginEnv, "attacker[bot]") // present in the HOST environment
	var names []string
	for _, kv := range procenv.BaseEnv() {
		name, _, _ := strings.Cut(kv, "=")
		names = append(names, name)
	}
	return names
}

// THE SEAM IS THE ONLY DOOR. #3885, #3890 and #3914 are the same defect three
// times: a GitHub provider constructed BESIDE the stage seam has no configured
// login, so its AuthenticatedLogin regresses to GET /user. Fixing the seam
// fixes it only for constructors that go through the seam, so this audits that
// no login-consuming command builds one directly.
//
// The allowlist is deliberately explicit: adding a direct construction is
// allowed, but it has to be a decision, and the file that makes it must not be
// one that asks a provider who it is.
func TestNoLoginConsumingCommandConstructsAGitHubProviderOffSeam(t *testing.T) {
	allowed := map[string]string{
		// The seam itself.
		"stageprovider.go": "the seam",
		"apireadcache.go":  "the cached wrapper the seam calls",
		// Non-stage or non-identity paths: none of these ever consults
		// AuthenticatedLogin, so no configured login is needed.
		"selfupdate.go":            "reads public releases; no repository identity",
		"tutorholdout.go":          "polls PR merge state; no identity comparison",
		"runnerwiring_counters.go": "daemon-side backlog counter; quota-gated reads",
		"fileissues.go":            "nomination publisher; files issues, never reads authorship",
		"setmilestone.go":          "sets a milestone; no authorship comparison",
		"publishbatch.go":          "publishes a batch; no authorship comparison",
	}

	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read cmd/goobers: %v", err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Join(".", name), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		var constructs, consultsIdentity bool
		ast.Inspect(file, func(n ast.Node) bool {
			switch node := n.(type) {
			case *ast.CallExpr:
				if ident, ok := node.Fun.(*ast.Ident); ok {
					if ident.Name == "newGitHubProvider" || ident.Name == "newCachedGitHubProvider" {
						constructs = true
					}
				}
				if sel, ok := node.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "AuthenticatedLogin" {
					consultsIdentity = true
				}
			}
			return true
		})
		if constructs && consultsIdentity {
			t.Errorf("%s both constructs a GitHub provider directly and consults AuthenticatedLogin: build it through newProviderForStage* so it carries the configured/stamped login (Goobers#3885/#3890/#3914)", name)
		}
		if constructs && allowed[name] == "" {
			t.Errorf("%s constructs a GitHub provider off-seam with no recorded reason; use newProviderForStage/newProviderForStageAs/newProviderForStageSurface, or add %s to the allowlist with the reason its provider needs no declared identity", name, name)
		}
	}
}
