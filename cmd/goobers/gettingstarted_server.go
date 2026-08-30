package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/providers"
)

// The guided endpoints expose the same product-owned onboarding operations as
// the CLI. Command-shaped actions execute this binary with an allowlisted argv;
// guided instance creation applies instance.GuidedOptions directly so the web
// wizard can submit structured choices without simulating terminal input.

const (
	guidedStateVersion    = 2
	guidedMaxBodyBytes    = 1 << 20
	guidedOutputRingLines = 500
	guidedRunIDMarker     = "created run "
	guidedJobKindRun      = "run"
)

var (
	// guidedExecCommand is the subprocess seam: tests stub it to assert exact
	// argv construction and to fake CLI output without spawning the real binary.
	guidedExecCommand = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		return exec.CommandContext(ctx, name, args...)
	}
	guidedSyncActionTimeout = 90 * time.Second
	guidedRunJobTimeout     = 10 * time.Minute
)

type guidedServer struct {
	workdir      string
	instancePath string
	configPath   string
	executable   string
	errorLog     *log.Logger

	mu       sync.Mutex
	job      *guidedJob
	api      http.Handler
	apiClose func() error
}

func newGuidedServer(workdir string, errorLog *log.Logger) (*guidedServer, error) {
	executable, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("resolve own executable: %w", err)
	}
	return &guidedServer{
		workdir:      workdir,
		instancePath: filepath.Join(workdir, gettingStartedInstanceDirName),
		configPath:   filepath.Join(workdir, gettingStartedConfigDirName),
		executable:   executable,
		errorLog:     errorLog,
	}, nil
}

func (s *guidedServer) close() error {
	s.mu.Lock()
	job := s.job
	apiClose := s.apiClose
	s.apiClose = nil
	s.mu.Unlock()
	if job != nil {
		job.stop()
	}
	if apiClose != nil {
		return apiClose()
	}
	return nil
}

// serveAPI serves the SAME standalone read-only API `goobers dashboard` builds,
// rooted at the tutorial instance, constructed lazily once instance.yaml
// exists. Until then every /api/ request is a 503 that tells the guide (and the
// user) what is missing.
func (s *guidedServer) serveAPI(w http.ResponseWriter, r *http.Request) {
	handler := s.apiHandler()
	if handler == nil {
		writeGuidedJSON(w, http.StatusServiceUnavailable, guidedErrorBody{
			Code:    "guided_no_instance",
			Message: "initialize the tutorial instance first",
		})
		return
	}
	handler.ServeHTTP(w, r)
}

func (s *guidedServer) apiHandler() http.Handler {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.api != nil {
		return s.api
	}
	layout := instance.NewLayout(s.instancePath)
	if _, err := os.Stat(layout.ConfigFile()); err != nil {
		return nil
	}
	config, err := instance.LoadConfig(layout.ConfigFile())
	if err != nil {
		s.errorLog.Printf("guided API: invalid tutorial instance.yaml: %v", err)
		return nil
	}
	// The guided onboarding server always binds loopback (see the
	// listenDashboard("127.0.0.1", ...) call in dashboard.go's getting-started
	// path), so the reveal-in-Finder action is always same-machine here.
	api, err := standaloneDashboardAPI(layout, config, s.errorLog, true)
	if err != nil {
		s.errorLog.Printf("guided API: initialize standalone read API: %v", err)
		return nil
	}
	s.api = api.handler
	s.apiClose = api.close
	return s.api
}

func (s *guidedServer) serveGuided(w http.ResponseWriter, r *http.Request) {
	if !guidedOriginAllowed(r) {
		writeGuidedJSON(w, http.StatusForbidden, guidedErrorBody{
			Code:    "origin_forbidden",
			Message: "cross-origin guided requests are forbidden",
		})
		return
	}
	switch {
	case r.URL.Path == "/guided/state":
		s.handleState(w, r)
	case r.URL.Path == "/guided/status":
		s.handleStatus(w, r)
	case r.URL.Path == "/guided/actions/init-instance":
		s.handleInitInstance(w, r)
	case r.URL.Path == "/guided/actions/inspect-repository":
		s.handleInspectRepository(w, r)
	case r.URL.Path == "/guided/actions/choose-repository-folder":
		s.handleChooseRepositoryFolder(w, r)
	case r.URL.Path == "/guided/actions/prepare-repository":
		s.handlePrepareRepository(w, r)
	case r.URL.Path == "/guided/actions/connect":
		s.handleConnect(w, r)
	case r.URL.Path == "/guided/actions/validate":
		s.handleValidate(w, r)
	case r.URL.Path == "/guided/actions/probe-backlog":
		s.handleProbeBacklog(w, r)
	case r.URL.Path == "/guided/actions/run":
		s.handleRun(w, r)
	case strings.HasPrefix(r.URL.Path, "/guided/jobs/"):
		s.handleJob(w, r)
	default:
		writeGuidedJSON(w, http.StatusNotFound, guidedErrorBody{
			Code:    "not_found",
			Message: "unknown guided endpoint",
		})
	}
}

type guidedErrorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type guidedEnvState struct {
	// TokenEnv is the RECORDED repository-token environment variable name —
	// the connect-time --token-env value persisted in instance.yaml, or
	// connectDefaultTokenEnv when the instance has not connected a repo yet.
	// GoobersGithubToken reports presence for exactly this name, in THIS
	// server process's environment. Never a client-supplied name: there is
	// no query parameter that lets a caller ask "is <arbitrary env var> set"
	// (#2639) — this is a presence check against one fixed, server-chosen
	// name, not a general environment-oracle endpoint.
	TokenEnv                 string `json:"tokenEnv"`
	GoobersGithubToken       bool   `json:"goobersGithubToken"`
	GoobersGithubIssuesToken bool   `json:"goobersGithubIssuesToken"`
}

type guidedJobSummary struct {
	ID       string  `json:"id"`
	Kind     string  `json:"kind"`
	Done     bool    `json:"done"`
	ExitCode *int    `json:"exitCode"`
	RunID    *string `json:"runId"`
}

type guidedJobDetail struct {
	guidedJobSummary
	Output []string `json:"output"`
}

type guidedStateBody struct {
	Version             int                  `json:"version"`
	Platform            string               `json:"platform"`
	Workdir             string               `json:"workdir"`
	InstancePath        string               `json:"instancePath"`
	ConfigPath          string               `json:"configPath"`
	SuggestedStack      string               `json:"suggestedStack,omitempty"`
	SuggestedCICommand  []string             `json:"suggestedCICommand,omitempty"`
	SuggestedCapability string               `json:"suggestedCapability,omitempty"`
	InstanceExists      bool                 `json:"instanceExists"`
	Env                 guidedEnvState       `json:"env"`
	Job                 *guidedJobSummary    `json:"job"`
	APIReady            bool                 `json:"apiReady"`
	Connected           guidedConnectedState `json:"connected"`
}

// guidedConnectedState reports the repository the tutorial instance is
// connected to. Repo is null until instance.yaml names a real (non-placeholder)
// repository — derived by the same loader `goobers connect` itself uses.
type guidedConnectedState struct {
	Repo *string `json:"repo"`
}

type guidedEnvelopeBody struct {
	ExitCode int             `json:"exitCode"`
	Envelope json.RawMessage `json:"envelope"`
	Stderr   string          `json:"stderr"`
}

type guidedInitBody struct {
	ExitCode int    `json:"exitCode"`
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
}

func (s *guidedServer) handleState(w http.ResponseWriter, r *http.Request) {
	if !requireGuidedMethod(w, r, http.MethodGet) {
		return
	}
	instanceExists := false
	if _, err := os.Stat(instance.NewLayout(s.instancePath).ConfigFile()); err == nil {
		instanceExists = true
	}
	connected := guidedConnectedState{}
	if repo := connectedRepository(s.instancePath); repo != "" {
		connected.Repo = &repo
	}
	s.mu.Lock()
	var job *guidedJobSummary
	if s.job != nil {
		summary := s.job.summary()
		job = &summary
	}
	apiReady := s.api != nil
	s.mu.Unlock()
	tokenEnv := s.recordedTokenEnv()
	suggestedStack, suggestedCICommand, suggestedCapability := detectCICommandDefault(s.workdir)
	writeGuidedJSON(w, http.StatusOK, guidedStateBody{
		Version:             guidedStateVersion,
		Platform:            runtime.GOOS,
		Workdir:             s.workdir,
		InstancePath:        s.instancePath,
		ConfigPath:          s.configPath,
		SuggestedStack:      suggestedStack,
		SuggestedCICommand:  suggestedCICommand,
		SuggestedCapability: suggestedCapability,
		InstanceExists:      instanceExists,
		Env: guidedEnvState{
			// Presence only — the values themselves never cross this API.
			TokenEnv:                 tokenEnv,
			GoobersGithubToken:       os.Getenv(tokenEnv) != "",
			GoobersGithubIssuesToken: os.Getenv(defaultWorkTrackingTokenEnv) != "",
		},
		Job:       job,
		APIReady:  apiReady,
		Connected: connected,
	})
}

// recordedTokenEnv is the one env var name /guided/state reports presence
// for and handleRun's preflight (below) checks — the connect-time
// --token-env value persisted in instance.yaml when the instance has
// connected a repository, otherwise the CLI's own default. This process's
// environment is fixed at launch (os.Environ() below is a snapshot, not a
// live view — see handleRun), so this always answers "does the server that
// is actually about to exec a run have this token," never "did the user's
// current shell export something" (#2639).
func (s *guidedServer) recordedTokenEnv() string {
	if env := connectedTokenEnv(s.instancePath); env != "" {
		return env
	}
	return connectDefaultTokenEnv
}

func (s *guidedServer) requiredRunTokenEnv() (string, bool) {
	configFile := instance.NewLayout(s.instancePath).ConfigFile()
	cfg, err := instance.LoadConfig(configFile)
	if err != nil {
		return s.recordedTokenEnv(), true
	}
	for _, repo := range cfg.Repos {
		if repo.Owner == connectPlaceholderOwner && repo.Name == connectPlaceholderName {
			continue
		}
		if repo.Provider == string(apiv1.ProviderADO) {
			if repo.Auth != nil && repo.Auth.Kind != instance.ADOAuthPAT {
				return "", false
			}
			return repo.Token.Env, repo.Token.Env != ""
		}
		if repo.GitHubAppAuth() || repo.Token.GitHubCLI != nil {
			return "", false
		}
		if repo.Token.Env != "" {
			return repo.Token.Env, true
		}
	}
	return s.recordedTokenEnv(), true
}

type guidedInitInstanceRequest struct {
	Template string                  `json:"template"`
	Guided   *guidedInitOptionsInput `json:"guided,omitempty"`
}

type guidedInitOptionsInput struct {
	Provider              string   `json:"provider,omitempty"`
	Owner                 string   `json:"owner,omitempty"`
	Project               string   `json:"project,omitempty"`
	Name                  string   `json:"name,omitempty"`
	LocalPath             string   `json:"localPath,omitempty"`
	ConfigPath            string   `json:"configPath,omitempty"`
	Repo                  string   `json:"repo"`
	Branch                string   `json:"branch"`
	Workflows             []string `json:"workflows"`
	IssueScope            string   `json:"issueScope"`
	AssignedTo            string   `json:"assignedTo,omitempty"`
	PullRequestCI         bool     `json:"pullRequestCI,omitempty"`
	CICommand             []string `json:"ciCommand,omitempty"`
	RequiredCapabilities  []string `json:"requiredCapabilities,omitempty"`
	Harness               string   `json:"harness"`
	RepoTokenEnv          string   `json:"repoTokenEnv"`
	WorkTrackingTokenEnv  string   `json:"workTrackingTokenEnv"`
	PullRequestTokenEnv   string   `json:"pullRequestTokenEnv,omitempty"`
	RepoPushTokenEnv      string   `json:"repoPushTokenEnv,omitempty"`
	OptionalModelTokenEnv string   `json:"optionalModelTokenEnv,omitempty"`
	GitHubCLIUser         string   `json:"githubCLIUser,omitempty"`
	AuthKind              string   `json:"authKind,omitempty"`
}

type guidedInspectRepositoryRequest struct {
	Location string `json:"location"`
}

type guidedChooseFolderResponse struct {
	Path     string `json:"path,omitempty"`
	Canceled bool   `json:"canceled"`
}

type guidedPrepareRepositoryRequest struct {
	Apply              bool `json:"apply"`
	CreateStarterIssue bool `json:"createStarterIssue"`
}

type guidedRepositoryReadiness struct {
	Provider            string   `json:"provider"`
	Repository          string   `json:"repository"`
	SelectorLabels      []string `json:"selectorLabels"`
	LifecycleLabels     []string `json:"lifecycleLabels"`
	MissingLabels       []string `json:"missingLabels"`
	CreatedLabels       []string `json:"createdLabels,omitempty"`
	EligibleCount       *int     `json:"eligibleCount,omitempty"`
	StarterIssueCreated bool     `json:"starterIssueCreated,omitempty"`
	UsesWorkItemTags    bool     `json:"usesWorkItemTags,omitempty"`
}

var guidedChooseRepositoryFolder = chooseGuidedRepositoryFolder

func (s *guidedServer) handleChooseRepositoryFolder(w http.ResponseWriter, r *http.Request) {
	if !requireGuidedMethod(w, r, http.MethodPost) {
		return
	}
	path, canceled, err := guidedChooseRepositoryFolder(r.Context())
	if err != nil {
		writeGuidedJSON(w, http.StatusInternalServerError, guidedErrorBody{
			Code:    "repository_folder_picker_failed",
			Message: err.Error(),
		})
		return
	}
	writeGuidedJSON(w, http.StatusOK, guidedChooseFolderResponse{
		Path:     path,
		Canceled: canceled,
	})
}

func (s *guidedServer) handlePrepareRepository(w http.ResponseWriter, r *http.Request) {
	if !requireGuidedMethod(w, r, http.MethodPost) {
		return
	}
	var input guidedPrepareRepositoryRequest
	if !decodeGuidedBody(w, r, &input) {
		return
	}
	layout := instance.NewLayout(s.instancePath)
	set, report, err := instance.LoadConfigDir(layout.ConfigDir())
	if err != nil {
		writeGuidedJSON(w, http.StatusConflict, guidedErrorBody{
			Code:    "guided_config_unavailable",
			Message: fmt.Sprintf("load generated configuration: %v (report: %+v)", err, report),
		})
		return
	}
	if len(set.Gaggles) == 0 {
		writeGuidedJSON(w, http.StatusConflict, guidedErrorBody{
			Code:    "guided_gaggle_unavailable",
			Message: "the generated configuration does not contain a gaggle",
		})
		return
	}
	gaggle := set.Gaggles[0]
	selectors, applied, _ := connectDerivedLabels(set, gaggle.Spec.Project.Owner, gaggle.Spec.Project.Name)
	response := guidedRepositoryReadiness{
		Provider:        string(gaggle.Spec.Project.Provider),
		Repository:      guidedRepositoryDisplayName(string(gaggle.Spec.Project.Provider), gaggle.Spec.Project.Owner, gaggle.Spec.Project.Project, gaggle.Spec.Project.Name),
		SelectorLabels:  selectors,
		LifecycleLabels: applied,
		MissingLabels:   []string{},
	}
	if gaggle.Spec.Project.Provider == apiv1.ProviderADO {
		response.UsesWorkItemTags = true
		writeGuidedJSON(w, http.StatusOK, response)
		return
	}

	token, err := guidedGitHubToken(r.Context())
	if err != nil {
		writeGuidedJSON(w, http.StatusBadRequest, guidedErrorBody{
			Code:    "github_credentials_unavailable",
			Message: err.Error(),
		})
		return
	}
	provider := providers.NewGitHubProvider(token)
	repository := providers.RepositoryRef{
		Provider: providers.ProviderGitHub,
		Owner:    gaggle.Spec.Project.Owner,
		Name:     gaggle.Spec.Project.Name,
	}
	catalog := connectSeedCatalog(selectors, applied)
	assignedTo := guidedImplementationAssignee(set)
	if len(catalog.Issues) > 0 {
		catalog.Issues[0].Assignee = assignedTo
	}
	existing, err := provider.RepositoryLabelNames(r.Context(), repository)
	if err != nil {
		writeGuidedJSON(w, http.StatusBadGateway, guidedErrorBody{
			Code:    "repository_labels_unavailable",
			Message: err.Error(),
		})
		return
	}
	response.MissingLabels = missingGuidedLabels(catalog.Labels, existing)
	reality, err := checkGuidedRepoSelectorReality(r.Context(), provider, repository, selectors, assignedTo)
	if err != nil {
		writeGuidedJSON(w, http.StatusBadGateway, guidedErrorBody{
			Code:    "repository_issues_unavailable",
			Message: err.Error(),
		})
		return
	}
	eligible := reality.Matching
	response.EligibleCount = &eligible
	if input.Apply {
		result := onboardingActionResult{Created: []string{}, Skipped: []string{}}
		if input.CreateStarterIssue && eligible == 0 {
			if err := seedOnboardingIssuesAs(r.Context(), provider, repository, catalog, connectAction, &result); err != nil {
				writeGuidedJSON(w, http.StatusBadGateway, guidedErrorBody{
					Code:    "repository_preparation_failed",
					Message: err.Error(),
				})
				return
			}
		} else {
			labels, err := provider.EnsureWorkItemLabels(r.Context(), repository, catalog.Labels)
			if err != nil {
				writeGuidedJSON(w, http.StatusBadGateway, guidedErrorBody{
					Code:    "repository_preparation_failed",
					Message: err.Error(),
				})
				return
			}
			for _, name := range labels.Created {
				result.Created = append(result.Created, "label:"+name)
			}
		}
		for _, item := range result.Created {
			switch {
			case strings.HasPrefix(item, "label:"):
				response.CreatedLabels = append(response.CreatedLabels, strings.TrimPrefix(item, "label:"))
			case strings.HasPrefix(item, "issue:"):
				response.StarterIssueCreated = true
			}
		}
		existing, err = provider.RepositoryLabelNames(r.Context(), repository)
		if err != nil {
			writeGuidedJSON(w, http.StatusBadGateway, guidedErrorBody{
				Code:    "repository_labels_unavailable",
				Message: err.Error(),
			})
			return
		}
		response.MissingLabels = missingGuidedLabels(catalog.Labels, existing)
		reality, err = checkGuidedRepoSelectorReality(r.Context(), provider, repository, selectors, assignedTo)
		if err != nil {
			writeGuidedJSON(w, http.StatusBadGateway, guidedErrorBody{
				Code:    "repository_issues_unavailable",
				Message: err.Error(),
			})
			return
		}
		eligible = reality.Matching
		response.EligibleCount = &eligible
	}
	writeGuidedJSON(w, http.StatusOK, response)
}

func guidedGitHubToken(ctx context.Context) (string, error) {
	for _, name := range []string{
		defaultWorkTrackingTokenEnv,
		connectDefaultTokenEnv,
		"GOOBERS_GITHUB_TOKEN",
	} {
		if token := strings.TrimSpace(os.Getenv(name)); token != "" {
			return token, nil
		}
	}
	token, err := runGuidedDiscovery(ctx, "gh", "auth", "token", "--hostname", "github.com")
	if err != nil || strings.TrimSpace(token) == "" {
		return "", errors.New("GitHub authentication is required to inspect and create repository labels; run `gh auth login` and restart setup")
	}
	return strings.TrimSpace(token), nil
}

func missingGuidedLabels(required []providers.WorkItemLabel, existing []string) []string {
	have := make(map[string]bool, len(existing))
	for _, name := range existing {
		have[strings.ToLower(strings.TrimSpace(name))] = true
	}
	var missing []string
	for _, label := range required {
		if !have[strings.ToLower(strings.TrimSpace(label.Name))] {
			missing = append(missing, label.Name)
		}
	}
	return missing
}

func guidedImplementationAssignee(set *instance.ConfigSet) string {
	for _, workflow := range set.Workflows {
		if workflow.Name != instance.GuidedWorkflowImplementation {
			continue
		}
		for _, task := range workflow.Spec.Tasks {
			if task.Name == "query-backlog" && task.Inputs["respectAssignee"] == "true" {
				return strings.TrimSpace(task.Inputs["assignedTo"])
			}
		}
	}
	return ""
}

func checkGuidedRepoSelectorReality(
	ctx context.Context,
	lister repoWorkItemLister,
	repository providers.RepositoryRef,
	selectors []string,
	assignedTo string,
) (repoSelectorReality, error) {
	if assignedTo == "" {
		return checkRepoSelectorReality(ctx, lister, repository, selectors)
	}
	items, err := lister.ListWorkItems(ctx, providers.ListWorkItemsRequest{
		Repository: repository,
		State:      "open",
		Assignee:   assignedTo,
		Limit:      repoSelectorRealitySample,
	})
	if err != nil {
		return repoSelectorReality{}, err
	}
	reality := repoSelectorReality{
		Selectors: normalizeRepoSelectors(selectors),
		Open:      len(items),
		Sampled:   len(items) >= repoSelectorRealitySample,
	}
	for _, item := range items {
		if repoItemMatchesSelectors(item, reality.Selectors) {
			reality.Matching++
		}
	}
	return reality, nil
}

func (s *guidedServer) handleInspectRepository(w http.ResponseWriter, r *http.Request) {
	if !requireGuidedMethod(w, r, http.MethodPost) {
		return
	}
	var input guidedInspectRepositoryRequest
	if !decodeGuidedBody(w, r, &input) {
		return
	}
	inspection, err := inspectGuidedRepository(r.Context(), input.Location)
	if err != nil {
		writeGuidedJSON(w, http.StatusBadRequest, guidedErrorBody{
			Code:    "repository_inspection_failed",
			Message: err.Error(),
		})
		return
	}
	writeGuidedJSON(w, http.StatusOK, inspection)
}

func (s *guidedServer) handleInitInstance(w http.ResponseWriter, r *http.Request) {
	if !requireGuidedMethod(w, r, http.MethodPost) {
		return
	}
	var input guidedInitInstanceRequest
	if !decodeGuidedBody(w, r, &input) {
		return
	}
	if input.Template == "guided" {
		s.handleGuidedInitInstance(w, r, input.Guided)
		return
	}
	// Allowlisted template chooser: "quickstart" (the tutorial default) execs
	// the templated init; "starter" execs the bare init, whose scaffold IS the
	// starter template. Anything else is a 400, never an argv.
	var argv []string
	switch input.Template {
	case "", "quickstart":
		argv = []string{"init", "--template=quickstart", s.instancePath}
	case "starter":
		argv = []string{"init", s.instancePath}
	default:
		writeGuidedJSON(w, http.StatusBadRequest, guidedErrorBody{
			Code:    "invalid_template",
			Message: "template must be \"quickstart\" or \"starter\"",
		})
		return
	}
	result, err := s.execSync(r.Context(), argv...)
	if err != nil {
		writeGuidedExecFailure(w, err)
		return
	}
	writeGuidedJSON(w, http.StatusOK, guidedInitBody{
		ExitCode: result.exitCode,
		Stdout:   result.stdout,
		Stderr:   result.stderr,
	})
}

func (s *guidedServer) handleGuidedInitInstance(w http.ResponseWriter, r *http.Request, input *guidedInitOptionsInput) {
	if input == nil {
		writeGuidedJSON(w, http.StatusBadRequest, guidedErrorBody{
			Code:    "missing_guided_options",
			Message: "guided setup options are required",
		})
		return
	}
	provider := strings.ToLower(strings.TrimSpace(input.Provider))
	repoOwner := strings.TrimSpace(input.Owner)
	repoProject := strings.TrimSpace(input.Project)
	repoName := strings.TrimSpace(input.Name)
	if provider == "" || repoOwner == "" || repoName == "" {
		identity, err := parseGuidedRepositoryIdentity(input.Repo)
		if err != nil {
			writeGuidedJSON(w, http.StatusBadRequest, guidedErrorBody{
				Code:    "invalid_repo",
				Message: err.Error(),
			})
			return
		}
		provider = identity.provider
		repoOwner = identity.owner
		repoProject = identity.project
		repoName = identity.name
	}
	assignedTo := strings.TrimSpace(input.AssignedTo)
	if strings.EqualFold(strings.TrimSpace(input.IssueScope), "assigned") && assignedTo == "" {
		auth := discoverGuidedAuth(r.Context(), guidedRepositoryIdentity{
			provider: provider,
			owner:    repoOwner,
			project:  repoProject,
			name:     repoName,
		})
		if !auth.Ready || strings.TrimSpace(auth.Identity) == "" {
			writeGuidedJSON(w, http.StatusBadRequest, guidedErrorBody{
				Code:    "assigned_identity_unavailable",
				Message: "could not resolve the authenticated repository identity; sign in to the provider CLI and inspect the repository again",
			})
			return
		}
		assignedTo = strings.TrimSpace(auth.Identity)
	}
	opts := instance.GuidedOptions{
		GaggleName:           guidedGaggleName(repoName),
		DisplayName:          guidedRepositoryDisplayName(provider, repoOwner, repoProject, repoName),
		RepoProvider:         provider,
		RepoOwner:            repoOwner,
		RepoProject:          repoProject,
		RepoName:             repoName,
		RepoBranch:           input.Branch,
		GitHubCLIUser:        input.GitHubCLIUser,
		RepoAuthKind:         input.AuthKind,
		RepoTokenEnv:         input.RepoTokenEnv,
		WorkTrackingTokenEnv: input.WorkTrackingTokenEnv,
		PullRequestTokenEnv:  input.PullRequestTokenEnv,
		RepoPushTokenEnv:     input.RepoPushTokenEnv,
		Harness:              input.Harness,
		Workflows:            append([]string(nil), input.Workflows...),
		IssueScope:           input.IssueScope,
		AssignedTo:           assignedTo,
		PullRequestCI:        input.PullRequestCI,
		CICommand:            append([]string(nil), input.CICommand...),
		RequiredCapabilities: append([]string(nil), input.RequiredCapabilities...),
	}
	if input.Harness == "claude-code" {
		opts.ClaudeTokenEnv = input.OptionalModelTokenEnv
	} else {
		opts.CopilotTokenEnv = input.OptionalModelTokenEnv
	}
	configPath := strings.TrimSpace(input.ConfigPath)
	if configPath == "" {
		configPath = s.configPath
	}
	configPath, err := filepath.Abs(configPath)
	if err != nil {
		writeGuidedJSON(w, http.StatusBadRequest, guidedErrorBody{
			Code:    "invalid_config_path",
			Message: fmt.Sprintf("resolve configuration path: %v", err),
		})
		return
	}
	if err := instance.CheckGuidedSourceInstancePaths(s.instancePath, configPath); err != nil {
		writeGuidedJSON(w, http.StatusBadRequest, guidedErrorBody{
			Code:    "invalid_config_path",
			Message: err.Error(),
		})
		return
	}
	if err := instance.CheckGuidedSourceTarget(configPath); err != nil {
		writeGuidedJSON(w, http.StatusConflict, guidedErrorBody{
			Code:    "config_source_conflict",
			Message: err.Error(),
		})
		return
	}
	if _, err := instance.SeedGuidedConfigSource(configPath, opts); err != nil {
		writeGuidedJSON(w, http.StatusBadRequest, guidedErrorBody{
			Code:    "invalid_guided_options",
			Message: err.Error(),
		})
		return
	}
	cfg, err := instance.LoadGuidedSourceConfig(configPath)
	if err != nil {
		writeGuidedJSON(w, http.StatusInternalServerError, guidedErrorBody{
			Code:    "load_guided_source_failed",
			Message: err.Error(),
		})
		return
	}
	result, err := instance.InitGuidedFromSource(s.instancePath, configPath, cfg)
	if err != nil {
		writeGuidedJSON(w, http.StatusConflict, guidedErrorBody{
			Code:    "guided_init_failed",
			Message: err.Error(),
		})
		return
	}
	s.mu.Lock()
	s.configPath = configPath
	s.mu.Unlock()
	writeGuidedJSON(w, http.StatusOK, guidedInitBody{
		ExitCode: 0,
		Stdout: fmt.Sprintf(
			"Created %s with %d workflow module(s) from %s.",
			result.Root,
			len(input.Workflows),
			configPath,
		),
	})
}

func guidedRepositoryDisplayName(provider, owner, project, name string) string {
	if provider == string(providers.ProviderADO) {
		return owner + "/" + project + "/" + name
	}
	return owner + "/" + name
}

type guidedConnectRequest struct {
	Repo     string `json:"repo"`
	TokenEnv string `json:"tokenEnv"`
	Seed     bool   `json:"seed"`
	Replace  bool   `json:"replace"`
}

// handleConnect wraps `goobers connect` exactly as a shell user would invoke
// it (docs/design/v1/cli-surface-and-manpages.md §5): repo/flags in, the
// parsed onboarding envelope out. Token VALUES never cross this API — only
// the environment variable name does, and the CLI's own paste-guard rejects
// pasted secrets.
func (s *guidedServer) handleConnect(w http.ResponseWriter, r *http.Request) {
	if !requireGuidedMethod(w, r, http.MethodPost) {
		return
	}
	var input guidedConnectRequest
	if !decodeGuidedBody(w, r, &input) {
		return
	}
	if input.Repo == "" {
		writeGuidedJSON(w, http.StatusBadRequest, guidedErrorBody{
			Code:    "invalid_repo",
			Message: "repo (owner/name) is required",
		})
		return
	}
	argv := []string{"connect", input.Repo, "--json"}
	if input.TokenEnv != "" {
		argv = append(argv, "--token-env", input.TokenEnv)
	}
	if input.Seed {
		argv = append(argv, "--seed")
	}
	if input.Replace {
		argv = append(argv, "--replace")
	}
	argv = append(argv, s.instancePath)
	s.respondEnvelope(w, r, argv)
}

type guidedValidateRequest struct {
	CheckHarness bool `json:"checkHarness"`
	CheckRepos   bool `json:"checkRepos"`
}

func (s *guidedServer) handleValidate(w http.ResponseWriter, r *http.Request) {
	if !requireGuidedMethod(w, r, http.MethodPost) {
		return
	}
	var input guidedValidateRequest
	if !decodeGuidedBody(w, r, &input) {
		return
	}
	argv := []string{"validate", "--json"}
	if input.CheckHarness {
		argv = append(argv, "--check-harness")
	}
	if input.CheckRepos {
		argv = append(argv, "--check-repos")
	}
	argv = append(argv, s.instancePath)
	s.respondEnvelope(w, r, argv)
}

// guidedProbeBody is a purpose-built envelope, not `backlog-query
// --read-only`'s raw output: that subcommand has no --json mode (adding one
// is out of #2638's scope — cmd/goobers/backlogquery.go is read-only,
// don't-modify evidence for this issue), so this handler parses its plain-
// text stdout itself and reports only what the wizard needs.
//
// EligibleCount is nil when the probe could not run at all (no issues token
// exported yet) — distinct from a real zero, so the wizard can tell "haven't
// checked" from "checked: none eligible".
type guidedProbeBody struct {
	ExitCode      int    `json:"exitCode"`
	EligibleCount *int   `json:"eligibleCount"`
	Stderr        string `json:"stderr"`
}

// handleProbeBacklog runs the SAME read-only eligibility scan the sample
// quickstart's query-backlog stage is about to run when dispatched
// (`goobers backlog-query --read-only`, cmd/goobers/backlogquery.go's
// runReadOnlyBacklogQuery — read-only, untouched by this issue), but before
// the run starts, so the wizard can warn "0 eligible issues" instead of
// letting the user watch a run complete with nothing to show for it (#2638).
//
// This is deliberately NOT how a workflow stage normally gets its
// capability-scoped credential (the runner injects GOOBERS_CRED_* env vars
// per declared capability, buildStageEnv) — there is no run yet to inject
// one. Standalone use is the documented fallback (providerToken's own error
// message: "or set %s directly for standalone use"), so this handler
// performs that same translation itself: the plain issues token the wizard
// already tracks (state.env.goobersGithubIssuesToken) into the
// capability-scoped var backlog-query's --read-only path reads.
func (s *guidedServer) handleProbeBacklog(w http.ResponseWriter, r *http.Request) {
	if !requireGuidedMethod(w, r, http.MethodGet) {
		return
	}
	token := os.Getenv(defaultWorkTrackingTokenEnv)
	if token == "" {
		token = os.Getenv("GOOBERS_GITHUB_TOKEN")
	}
	if token == "" {
		// No token exported yet — this is a normal, early wizard state (the
		// "export the token" step hasn't happened), not a probe failure.
		writeGuidedJSON(w, http.StatusOK, guidedProbeBody{})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), guidedSyncActionTimeout)
	defer cancel()
	command := guidedExecCommand(ctx, s.executable, "backlog-query", "--read-only", s.instancePath)
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	command.Env = append(
		os.Environ(),
		executor.CredentialEnvVar(string(capability.GitHubIssuesRead))+"="+token,
		// The sample quickstart's own query-backlog stage inputs
		// (internal/instance/quickstart-v1/gaggles/example/workflows/quickstart.yaml)
		// — mirrored here (#2638) so this probe asks the exact same question
		// that stage is about to ask when the run actually dispatches, sourced
		// through the same providers label constants the stage-name lint wants
		// rather than as bare literals. Sample-path only: an own-repo
		// connect's label conventions are the user's own (#2609's `goobers
		// connect --seed` choreography), not a fixed pair this server can
		// assume.
		executor.InputEnvVar("trustLabel")+"="+providers.LabelApproved,
		executor.InputEnvVar("requireLabels")+"="+providers.LabelReady,
	)
	err := command.Run()
	exitCode := 0
	if err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			writeGuidedExecFailure(w, err)
			return
		}
		exitCode = exitErr.ExitCode()
	}
	count := countEligibleBacklogLines(stdout.String())
	writeGuidedJSON(w, http.StatusOK, guidedProbeBody{
		ExitCode:      exitCode,
		EligibleCount: &count,
		Stderr:        stderr.String(),
	})
}

// countEligibleBacklogLines parses runReadOnlyBacklogQuery's plain-text
// stdout (cmd/goobers/backlogquery.go): "no eligible items" on its own line
// when the scan found nothing, otherwise one "ID\tTitle" line per eligible
// item. That contract is a comment away from this function, not a
// compile-time link, but it's read-only production behavior (#233) that
// isn't expected to change casually.
func countEligibleBacklogLines(stdout string) int {
	trimmed := strings.TrimSpace(stdout)
	if trimmed == "" || trimmed == "no eligible items" {
		return 0
	}
	return len(strings.Split(trimmed, "\n"))
}

func (s *guidedServer) handleStatus(w http.ResponseWriter, r *http.Request) {
	if !requireGuidedMethod(w, r, http.MethodGet) {
		return
	}
	s.respondEnvelope(w, r, []string{"status", "--json", s.instancePath})
}

type guidedRunRequest struct {
	Workflow string `json:"workflow"`
}

func (s *guidedServer) handleRun(w http.ResponseWriter, r *http.Request) {
	if !requireGuidedMethod(w, r, http.MethodPost) {
		return
	}
	var input guidedRunRequest
	if !decodeGuidedBody(w, r, &input) {
		return
	}
	// Allowlisted workflow chooser: the tutorial templates plus the canonical
	// modules created by guided setup. Anything else is a 400, never an argv.
	workflow := input.Workflow
	switch workflow {
	case "":
		workflow = "quickstart"
	case "quickstart", "default-implement",
		instance.GuidedWorkflowImplementation,
		instance.GuidedWorkflowBacklogCuration,
		instance.GuidedWorkflowWorkNomination:
	default:
		writeGuidedJSON(w, http.StatusBadRequest, guidedErrorBody{
			Code:    "invalid_workflow",
			Message: "workflow is not available from guided setup",
		})
		return
	}
	// Credential preflight (#2639): the subprocess below inherits THIS
	// process's environment, fixed at server launch — no export a user runs
	// afterward, in any shell, can ever reach it. Checking here, synchronously,
	// before a job is even created, turns a run that would silently fail deep
	// inside the CLI (or worse, dispatch and let the workflow's own git-auth
	// step fail unhelpfully) into one actionable error at the point the
	// server actually knows the credential is missing.
	tokenEnv, tokenRequired := s.requiredRunTokenEnv()
	if tokenRequired && os.Getenv(tokenEnv) == "" {
		writeGuidedJSON(w, http.StatusBadRequest, guidedErrorBody{
			Code: "token_env_unset",
			Message: fmt.Sprintf(
				"%s is not set in the getting-started server's own process — export it in the shell that runs \"goobers getting-started\" and restart the server; a later export in a different shell cannot reach an already-running process",
				tokenEnv,
			),
		})
		return
	}
	s.mu.Lock()
	if s.job != nil && !s.job.isDone() {
		s.mu.Unlock()
		writeGuidedJSON(w, http.StatusConflict, guidedErrorBody{
			Code:    "job_running",
			Message: "a guided job is already running",
		})
		return
	}
	job := newGuidedJob(guidedJobKindRun)
	s.job = job
	s.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), guidedRunJobTimeout)
	job.cancel = cancel
	command := guidedExecCommand(ctx, s.executable, "run", workflow, s.instancePath)
	// stdout and stderr interleave into the job's bounded ring, exactly as a
	// terminal user would see them.
	command.Stdout = job
	command.Stderr = job
	// The subprocess inherits this process's environment — a snapshot fixed
	// at server launch, NOT a live view of any shell. A token exported after
	// the server started, in any shell (including the one that launched it),
	// is not in os.Environ() here; only a token exported before launch, or a
	// server restart after exporting, changes what this actually contains.
	// The preflight above already refused to reach this line if the recorded
	// token env is unset in this snapshot.
	command.Env = os.Environ()
	go func() {
		defer cancel()
		err := command.Run()
		code := 0
		if err != nil {
			var exitErr *exec.ExitError
			if errors.As(err, &exitErr) {
				code = exitErr.ExitCode()
			} else {
				code = -1
				_, _ = io.WriteString(job, "error: "+err.Error()+"\n")
			}
		}
		job.finish(code)
	}()
	writeGuidedJSON(w, http.StatusAccepted, map[string]string{"jobId": job.id})
}

func (s *guidedServer) handleJob(w http.ResponseWriter, r *http.Request) {
	if !requireGuidedMethod(w, r, http.MethodGet) {
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/guided/jobs/")
	s.mu.Lock()
	job := s.job
	s.mu.Unlock()
	if job == nil || id == "" || job.id != id {
		writeGuidedJSON(w, http.StatusNotFound, guidedErrorBody{
			Code:    "job_not_found",
			Message: "no guided job with that id",
		})
		return
	}
	writeGuidedJSON(w, http.StatusOK, job.detail())
}

type guidedExecResult struct {
	exitCode int
	stdout   string
	stderr   string
}

// execSync runs one synchronous CLI action with the shared timeout. The
// returned error is a start failure only; a nonzero exit is a normal result.
func (s *guidedServer) execSync(parent context.Context, argv ...string) (guidedExecResult, error) {
	ctx, cancel := context.WithTimeout(parent, guidedSyncActionTimeout)
	defer cancel()
	command := guidedExecCommand(ctx, s.executable, argv...)
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	command.Env = os.Environ()
	err := command.Run()
	result := guidedExecResult{stdout: stdout.String(), stderr: stderr.String()}
	if err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			return guidedExecResult{}, err
		}
		result.exitCode = exitErr.ExitCode()
	}
	return result, nil
}

func (s *guidedServer) respondEnvelope(w http.ResponseWriter, r *http.Request, argv []string) {
	result, err := s.execSync(r.Context(), argv...)
	if err != nil {
		writeGuidedExecFailure(w, err)
		return
	}
	envelope := json.RawMessage("null")
	if trimmed := bytes.TrimSpace([]byte(result.stdout)); len(trimmed) > 0 && json.Valid(trimmed) {
		envelope = json.RawMessage(trimmed)
	}
	writeGuidedJSON(w, http.StatusOK, guidedEnvelopeBody{
		ExitCode: result.exitCode,
		Envelope: envelope,
		Stderr:   result.stderr,
	})
}

func writeGuidedExecFailure(w http.ResponseWriter, err error) {
	writeGuidedJSON(w, http.StatusInternalServerError, guidedErrorBody{
		Code:    "exec_failed",
		Message: err.Error(),
	})
}

// guidedJob is the single-slot async job. Its Write method is the interleaved
// stdout+stderr sink: a bounded ring of whole lines, scanned for the CLI's
// "created run <id>" marker.
type guidedJob struct {
	id     string
	kind   string
	cancel context.CancelFunc

	mu       sync.Mutex
	lines    []string
	partial  []byte
	done     bool
	exitCode *int
	runID    *string
}

func newGuidedJob(kind string) *guidedJob {
	raw := make([]byte, 8)
	_, _ = rand.Read(raw)
	return &guidedJob{id: hex.EncodeToString(raw), kind: kind}
}

func (j *guidedJob) Write(data []byte) (int, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.partial = append(j.partial, data...)
	for {
		newline := bytes.IndexByte(j.partial, '\n')
		if newline < 0 {
			break
		}
		line := strings.TrimRight(string(j.partial[:newline]), "\r")
		j.partial = j.partial[newline+1:]
		j.appendLineLocked(line)
	}
	return len(data), nil
}

func (j *guidedJob) appendLineLocked(line string) {
	j.lines = append(j.lines, line)
	if len(j.lines) > guidedOutputRingLines {
		j.lines = j.lines[len(j.lines)-guidedOutputRingLines:]
	}
	if j.runID == nil && strings.HasPrefix(line, guidedRunIDMarker) {
		fields := strings.Fields(strings.TrimPrefix(line, guidedRunIDMarker))
		if len(fields) > 0 {
			runID := fields[0]
			j.runID = &runID
		}
	}
}

func (j *guidedJob) finish(exitCode int) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if len(j.partial) > 0 {
		j.appendLineLocked(string(j.partial))
		j.partial = nil
	}
	j.exitCode = &exitCode
	j.done = true
}

func (j *guidedJob) stop() {
	if j.cancel != nil {
		j.cancel()
	}
}

func (j *guidedJob) isDone() bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.done
}

func (j *guidedJob) summary() guidedJobSummary {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.summaryLocked()
}

func (j *guidedJob) summaryLocked() guidedJobSummary {
	summary := guidedJobSummary{ID: j.id, Kind: j.kind, Done: j.done}
	if j.exitCode != nil {
		code := *j.exitCode
		summary.ExitCode = &code
	}
	if j.runID != nil {
		runID := *j.runID
		summary.RunID = &runID
	}
	return summary
}

func (j *guidedJob) detail() guidedJobDetail {
	j.mu.Lock()
	defer j.mu.Unlock()
	output := make([]string, len(j.lines))
	copy(output, j.lines)
	return guidedJobDetail{guidedJobSummary: j.summaryLocked(), Output: output}
}

func requireGuidedMethod(w http.ResponseWriter, r *http.Request, method string) bool {
	if r.Method != method {
		w.Header().Set("Allow", method)
		writeGuidedJSON(w, http.StatusMethodNotAllowed, guidedErrorBody{
			Code:    "method_not_allowed",
			Message: "method not allowed",
		})
		return false
	}
	return true
}

// decodeGuidedBody enforces the POST transport rules — Content-Type
// application/json, a 1MB body cap, and DisallowUnknownFields — mirroring
// internal/httpapi's mutation transport. An empty body decodes as the zero
// request.
func decodeGuidedBody(w http.ResponseWriter, r *http.Request, into any) bool {
	contentType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || !strings.EqualFold(contentType, "application/json") {
		writeGuidedJSON(w, http.StatusUnsupportedMediaType, guidedErrorBody{
			Code:    "unsupported_media_type",
			Message: "Content-Type must be application/json",
		})
		return false
	}
	defer func() { _ = r.Body.Close() }()
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, guidedMaxBodyBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(into); err != nil {
		if errors.Is(err, io.EOF) {
			return true
		}
		writeGuidedJSON(w, http.StatusBadRequest, guidedErrorBody{
			Code:    "invalid_body",
			Message: "invalid JSON request body: " + err.Error(),
		})
		return false
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		writeGuidedJSON(w, http.StatusBadRequest, guidedErrorBody{
			Code:    "invalid_body",
			Message: "request body must be a single JSON value",
		})
		return false
	}
	return true
}

// guidedOriginAllowed mirrors internal/httpapi/mutations.go's origin check:
// when an Origin header is present it must be a bare loopback http(s) origin
// matching the request's own loopback host.
func guidedOriginAllowed(r *http.Request) bool {
	origin := strings.TrimSpace(r.Header.Get("Origin"))
	if origin == "" {
		return true
	}
	parsed, err := url.Parse(origin)
	if err != nil ||
		parsed.User != nil ||
		(parsed.Scheme != "http" && parsed.Scheme != "https") ||
		parsed.Host == "" ||
		parsed.Path != "" ||
		parsed.RawQuery != "" ||
		parsed.Fragment != "" ||
		!guidedLoopbackAuthority(parsed.Host) ||
		!guidedLoopbackAuthority(r.Host) ||
		!strings.EqualFold(parsed.Host, r.Host) {
		return false
	}
	return true
}

func guidedLoopbackAuthority(authority string) bool {
	host := authority
	if parsedHost, _, err := net.SplitHostPort(authority); err == nil {
		host = parsedHost
	}
	host = strings.Trim(host, "[]")
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func writeGuidedJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
