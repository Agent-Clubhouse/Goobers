package workerhost

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"

	"github.com/goobers/goobers/internal/workerrollout"
	"github.com/goobers/goobers/internal/workerrouting"
)

// #5950: versioning is opt-in. A worker with a build version but no opt-in
// polls unversioned, which is what a fresh reference Temporal routes to; a
// versioned default stalls the engine on every upgrade that changes the build.
func TestWorkerOptionsUnversionedUnlessOptedIn(t *testing.T) {
	h, err := New(Config{TaskQueues: []string{"goobers-engine"}, BuildVersion: "v0.5.0"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	opts := h.workerOptions()
	if opts.DeploymentOptions.UseVersioning {
		t.Fatal("DeploymentOptions.UseVersioning = true without the engine.workerVersioning opt-in, want false")
	}
	if opts.DeploymentOptions.DefaultVersioningBehavior != 0 {
		t.Fatalf("DefaultVersioningBehavior = %v on an unversioned worker; the SDK rejects that combination", opts.DeploymentOptions.DefaultVersioningBehavior)
	}
	if !strings.Contains(opts.Identity, "goobers-worker/v0.5.0@") {
		t.Fatalf("identity %q lost the build version; it must stay diagnosable when unversioned", opts.Identity)
	}
}

func TestWorkerOptionsUseControllerDeploymentVersion(t *testing.T) {
	t.Setenv(temporalDeploymentNameEnv, DeploymentName)
	t.Setenv(temporalWorkerBuildIDEnv, "release-v0.9.0")
	t.Setenv(podNamespaceEnv, "goobers-system")
	h, err := New(Config{
		TaskQueues:           []string{"goobers-engine"},
		BuildVersion:         "v0.9.0",
		AutoUpgradeRetention: true,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	opts := h.workerOptions()
	if !opts.DeploymentOptions.UseVersioning ||
		opts.DeploymentOptions.Version.DeploymentName != DeploymentName ||
		opts.DeploymentOptions.Version.BuildID != "release-v0.9.0" {
		t.Fatalf("DeploymentOptions = %+v, want controller-injected version", opts.DeploymentOptions)
	}
	if !strings.Contains(opts.Identity, "goobers-worker/release-v0.9.0@") {
		t.Fatalf("identity %q does not use controller build ID", opts.Identity)
	}
	if h.cfg.VersionRetention != workerrollout.DefaultVersionLimit || h.cfg.RolloutNamespace != "goobers-system" {
		t.Fatalf("controller rollout config = retention %d namespace %q", h.cfg.VersionRetention, h.cfg.RolloutNamespace)
	}
	if h.cfg.AutoUpgradeRetention {
		t.Fatal("AUTO_UPGRADE retention enabled without compatibility attestation")
	}
}

func TestWorkerControllerAutoUpgradeRetentionAttestationIsBuildBound(t *testing.T) {
	for _, tc := range []struct {
		name        string
		attestation string
		wantEnabled bool
	}{
		{name: "current build", attestation: "template-hash", wantEnabled: true},
		{name: "stale build", attestation: "previous-hash"},
		{name: "legacy boolean", attestation: "true"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(temporalDeploymentNameEnv, DeploymentName)
			t.Setenv(temporalWorkerBuildIDEnv, "template-hash")
			t.Setenv(podNamespaceEnv, "goobers-system")
			t.Setenv(autoUpgradeRetentionEnv, tc.attestation)
			sink := &logSink{}
			h, err := New(Config{TaskQueues: []string{"goobers-engine"}, Logf: sink.logf})
			if err != nil {
				t.Fatal(err)
			}
			if h.cfg.AutoUpgradeRetention != tc.wantEnabled {
				t.Fatalf("AutoUpgradeRetention = %t, want %t", h.cfg.AutoUpgradeRetention, tc.wantEnabled)
			}
			lines := sink.snapshot()
			if tc.wantEnabled {
				if len(lines) != 0 {
					t.Fatalf("valid attestation logged %q", lines)
				}
				return
			}
			if len(lines) != 1 ||
				!strings.Contains(lines[0], autoUpgradeRetentionEnv+"="+fmt.Sprintf("%q", tc.attestation)) ||
				!strings.Contains(lines[0], temporalWorkerBuildIDEnv+`="template-hash"`) ||
				!strings.Contains(lines[0], "retention is disabled") {
				t.Fatalf("mismatch log = %q", lines)
			}
		})
	}
}

func TestWorkerControllerDeploymentVersionFailsClosed(t *testing.T) {
	for _, tc := range []struct {
		name       string
		deployment string
		build      string
	}{
		{name: "missing build", deployment: DeploymentName},
		{name: "missing deployment", build: "v2"},
		{name: "wrong deployment", deployment: "other", build: "v2"},
		{name: "placeholder build", deployment: DeploymentName, build: "CHANGE-ME-release-version"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(temporalDeploymentNameEnv, tc.deployment)
			t.Setenv(temporalWorkerBuildIDEnv, tc.build)
			if _, err := New(Config{TaskQueues: []string{"goobers-engine"}}); err == nil {
				t.Fatal("New accepted incomplete or foreign controller identity")
			}
		})
	}
}

func TestWorkerControllerRejectsPlaceholderAttestation(t *testing.T) {
	t.Setenv(temporalDeploymentNameEnv, DeploymentName)
	t.Setenv(temporalWorkerBuildIDEnv, "release-v2")
	t.Setenv(podNamespaceEnv, "goobers-system")
	t.Setenv(autoUpgradeRetentionEnv, "CHANGE-ME-replay-qualified-release")
	if _, err := New(Config{TaskQueues: []string{"goobers-engine"}}); err == nil ||
		!strings.Contains(err.Error(), "placeholder") {
		t.Fatalf("New placeholder attestation error = %v", err)
	}
}

func TestWorkerControllerBuildIDMustContainReleaseBinaryVersion(t *testing.T) {
	for _, tc := range []struct {
		name    string
		buildID string
		wantErr bool
	}{
		{name: "reused previous release", buildID: "release-0.8.0", wantErr: true},
		{name: "matching release", buildID: "release-0.9.0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(temporalDeploymentNameEnv, DeploymentName)
			t.Setenv(temporalWorkerBuildIDEnv, tc.buildID)
			t.Setenv(podNamespaceEnv, "goobers-system")
			_, err := New(Config{TaskQueues: []string{"goobers-engine"}, BuildVersion: "0.9.0"})
			if (err != nil) != tc.wantErr {
				t.Fatalf("New(build=%q, binary=0.9.0) error = %v, wantErr=%t", tc.buildID, err, tc.wantErr)
			}
			if tc.wantErr && !strings.Contains(err.Error(), "does not contain release binary version") {
				t.Fatalf("mismatch error = %v", err)
			}
		})
	}
}

func deploymentVersion(build string) *worker.WorkerDeploymentVersion {
	return &worker.WorkerDeploymentVersion{DeploymentName: DeploymentName, BuildID: build}
}

func TestRoutingProblem(t *testing.T) {
	describeErr := errors.New("worker deployment not found")
	cases := []struct {
		name      string
		versioned bool
		routing   client.WorkerDeploymentRoutingConfig
		err       error
		want      string // substring; "" means healthy
	}{
		{name: "versioned current", versioned: true, routing: client.WorkerDeploymentRoutingConfig{CurrentVersion: deploymentVersion("v2")}},
		{name: "versioned ramping", versioned: true, routing: client.WorkerDeploymentRoutingConfig{
			CurrentVersion: deploymentVersion("v1"), RampingVersion: deploymentVersion("v2"), RampingVersionPercentage: 10,
		}},
		// The #5950 observation: current stayed unversioned while the new
		// worker polled as a version. Polls succeed and receive nothing.
		{name: "versioned but current unversioned", versioned: true, routing: client.WorkerDeploymentRoutingConfig{},
			want: "current version is __unversioned__ but this worker serves goobers.v2"},
		{name: "versioned but current is previous build", versioned: true, routing: client.WorkerDeploymentRoutingConfig{CurrentVersion: deploymentVersion("v1")},
			want: "set-current-version --deployment-name goobers --build-id v2"},
		{name: "versioned, paused ramp", versioned: true, routing: client.WorkerDeploymentRoutingConfig{
			CurrentVersion: deploymentVersion("v1"), RampingVersion: deploymentVersion("v2"),
		}, want: "current version is goobers.v1"},
		{name: "versioned, describe fails", versioned: true, err: describeErr, want: "cannot verify"},
		{name: "unversioned, no deployment", err: describeErr},
		{name: "unversioned, current unversioned", routing: client.WorkerDeploymentRoutingConfig{}},
		// An instance that ran versioned and upgraded to the unversioned
		// default: the deployment still routes to its old version.
		{name: "unversioned but current versioned", routing: client.WorkerDeploymentRoutingConfig{CurrentVersion: deploymentVersion("dev")},
			want: "current version is goobers.dev, so this UNVERSIONED worker receives no new workflow tasks"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := workerrouting.Check(tc.versioned, "v2", workerrouting.Routing{WorkerDeploymentRoutingConfig: tc.routing}, tc.err).Problem
			if tc.want == "" {
				if got != "" {
					t.Fatalf("Check problem = %q, want healthy", got)
				}
				return
			}
			if !strings.Contains(got, tc.want) {
				t.Fatalf("Check problem = %q, want it to contain %q", got, tc.want)
			}
		})
	}
}

type logSink struct {
	mu    sync.Mutex
	lines []string
}

func (l *logSink) logf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
}

func (l *logSink) snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.lines...)
}

// The running worker reports a mismatch while it lasts and its resolution
// once, and never fails over it.
func TestRunReportsRoutingMismatchWithoutFailing(t *testing.T) {
	sink := &logSink{}
	fleet := &fakeFleet{}
	h := newTestHost(t, Config{
		TaskQueues:   []string{"goobers-engine"},
		BuildVersion: "v2",
		Versioning:   true,
		Logf:         sink.logf,
	}, fleet)
	h.routingDelay = time.Millisecond
	h.routingInterval = time.Millisecond
	var mu sync.Mutex
	calls := 0
	h.describeRouting = func(context.Context, client.Client) (workerrouting.Routing, error) {
		mu.Lock()
		defer mu.Unlock()
		calls++
		if calls <= 2 {
			return workerrouting.Routing{WorkerDeploymentRoutingConfig: client.WorkerDeploymentRoutingConfig{CurrentVersion: deploymentVersion("v1")}}, nil
		}
		return workerrouting.Routing{WorkerDeploymentRoutingConfig: client.WorkerDeploymentRoutingConfig{CurrentVersion: deploymentVersion("v2")}}, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- h.Run(ctx) }()
	waitFor(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return calls >= 4
	})
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}

	lines := sink.snapshot()
	var mismatches, resolved int
	for _, line := range lines {
		switch {
		case strings.Contains(line, "error:") && strings.Contains(line, "current version is goobers.v1"):
			mismatches++
		case strings.Contains(line, "now routes tasks to this worker"):
			resolved++
		}
	}
	if mismatches != 2 || resolved != 1 {
		t.Fatalf("logged %d mismatch and %d resolution line(s), want 2 and 1: %q", mismatches, resolved, lines)
	}
}

// A worker whose version was replaced keeps polling while its pinned
// workflows drain (#7427 review): that is expected, so it logs no error and
// no command that would revert the rollout.
func TestRoutingCheckQuietWhileDraining(t *testing.T) {
	sink := &logSink{}
	h := newTestHost(t, Config{TaskQueues: []string{"goobers-engine"}, BuildVersion: "v1", Versioning: true, Logf: sink.logf}, &fakeFleet{})
	draining := workerrouting.Routing{
		WorkerDeploymentRoutingConfig: client.WorkerDeploymentRoutingConfig{CurrentVersion: deploymentVersion("v2")},
		Draining:                      map[string]bool{"v1": true},
	}
	h.describeRouting = func(context.Context, client.Client) (workerrouting.Routing, error) { return draining, nil }
	if reported := h.checkRoutingOnce(context.Background(), nil, ""); reported != "" {
		t.Fatalf("draining worker reported %q", reported)
	}
	if reported := h.checkRoutingOnce(context.Background(), nil, "earlier mismatch"); reported != "" {
		t.Fatalf("draining worker reported %q", reported)
	}
	lines := sink.snapshot()
	if len(lines) != 1 || !strings.Contains(lines[0], "it is draining") || strings.Contains(lines[0], "error") {
		t.Fatalf("logged %q, want one draining note after the earlier mismatch", lines)
	}
}

func TestWorkerReadinessFailsWhenUnrouted(t *testing.T) {
	h := newTestHost(t, Config{
		TaskQueues:   []string{"goobers-engine"},
		BuildVersion: "v2",
		Versioning:   true,
	}, &fakeFleet{})
	h.describeRouting = func(context.Context, client.Client) (workerrouting.Routing, error) {
		return workerrouting.Routing{
			WorkerDeploymentRoutingConfig: client.WorkerDeploymentRoutingConfig{
				CurrentVersion: deploymentVersion("v2"),
			},
		}, nil
	}

	h.checkRoutingOnce(context.Background(), nil, "")
	response := httptest.NewRecorder()
	h.healthHandler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("GET /readyz = %d, want %d before pollers start", response.Code, http.StatusServiceUnavailable)
	}

	h.pollersStarted.Store(true)
	h.describeRouting = func(context.Context, client.Client) (workerrouting.Routing, error) {
		return workerrouting.Routing{}, nil
	}
	h.checkRoutingOnce(context.Background(), nil, "")
	response = httptest.NewRecorder()
	h.healthHandler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("GET /readyz = %d, want %d while the build is unregistered", response.Code, http.StatusServiceUnavailable)
	}

	h.describeRouting = func(context.Context, client.Client) (workerrouting.Routing, error) {
		return workerrouting.Routing{
			WorkerDeploymentRoutingConfig: client.WorkerDeploymentRoutingConfig{
				CurrentVersion: deploymentVersion("v1"),
			},
		}, nil
	}
	h.checkRoutingOnce(context.Background(), nil, "unregistered")
	response = httptest.NewRecorder()
	h.healthHandler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("GET /readyz = %d, want %d while current routes to another build", response.Code, http.StatusServiceUnavailable)
	}

	h.describeRouting = func(context.Context, client.Client) (workerrouting.Routing, error) {
		return workerrouting.Routing{
			WorkerDeploymentRoutingConfig: client.WorkerDeploymentRoutingConfig{
				CurrentVersion: deploymentVersion("v2"),
			},
		}, nil
	}
	h.checkRoutingOnce(context.Background(), nil, "unrouted")
	response = httptest.NewRecorder()
	h.healthHandler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("GET /readyz = %d, want %d after routing reaches this build", response.Code, http.StatusOK)
	}
	if strings.Contains(response.Body.String(), `"draining":true`) {
		t.Fatalf("GET /readyz body = %s, want current build not draining", response.Body.String())
	}
}

func TestWorkerReadinessKeepsDrainingVersionReady(t *testing.T) {
	h := newTestHost(t, Config{
		TaskQueues:   []string{"goobers-engine"},
		BuildVersion: "v1",
		Versioning:   true,
	}, &fakeFleet{})
	h.pollersStarted.Store(true)
	h.describeRouting = func(context.Context, client.Client) (workerrouting.Routing, error) {
		return workerrouting.Routing{
			WorkerDeploymentRoutingConfig: client.WorkerDeploymentRoutingConfig{
				CurrentVersion: deploymentVersion("v2"),
			},
			Draining: map[string]bool{"v1": true},
		}, nil
	}

	h.checkRoutingOnce(context.Background(), nil, "")
	response := httptest.NewRecorder()
	h.healthHandler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("GET /readyz = %d, want %d for draining build with pinned work", response.Code, http.StatusOK)
	}
	if !strings.Contains(response.Body.String(), `"draining":true`) {
		t.Fatalf("GET /readyz body = %s, want draining=true", response.Body.String())
	}
}

func TestRainbowWorkerReadinessRequiresRegistrationNotRouting(t *testing.T) {
	for _, tc := range []struct {
		name         string
		routing      workerrouting.Routing
		want         int
		wantInterval time.Duration
	}{
		{
			name: "first install registered without current",
			routing: workerrouting.Routing{
				Registered: map[string]bool{"v2": true},
			},
			want:         http.StatusOK,
			wantInterval: routingCheckInterval,
		},
		{
			name: "newer registered build not current",
			routing: workerrouting.Routing{
				WorkerDeploymentRoutingConfig: client.WorkerDeploymentRoutingConfig{
					CurrentVersion: deploymentVersion("v1"),
				},
				Registered: map[string]bool{"v1": true, "v2": true},
			},
			want:         http.StatusOK,
			wantInterval: routingCheckInterval,
		},
		{
			name: "unregistered build",
			routing: workerrouting.Routing{
				WorkerDeploymentRoutingConfig: client.WorkerDeploymentRoutingConfig{
					CurrentVersion: deploymentVersion("v1"),
				},
				Registered: map[string]bool{"v1": true},
			},
			want:         http.StatusServiceUnavailable,
			wantInterval: routingRegistrationCheckInterval,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newTestHost(t, Config{
				TaskQueues:       []string{"goobers-engine"},
				BuildVersion:     "v2",
				Versioning:       true,
				VersionRetention: workerrollout.DefaultVersionLimit,
				RolloutNamespace: "goobers-system",
			}, &fakeFleet{})
			h.pollersStarted.Store(true)
			h.describeRouting = func(context.Context, client.Client) (workerrouting.Routing, error) {
				return tc.routing, nil
			}
			if problem := h.checkRoutingOnce(context.Background(), nil, ""); problem == "" && tc.want == http.StatusOK {
				t.Fatal("registered inactive build lost its routing mismatch diagnostic")
			}
			response := httptest.NewRecorder()
			h.healthHandler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/readyz", nil))
			if response.Code != tc.want {
				t.Fatalf("GET /readyz = %d, want %d; body=%s", response.Code, tc.want, response.Body.String())
			}
			if interval := h.nextRoutingCheckInterval(0); interval != tc.wantInterval {
				t.Fatalf("next routing check = %s, want %s", interval, tc.wantInterval)
			}
		})
	}

}

func TestRoutingDescribeErrorsBackOffExponentially(t *testing.T) {
	h := newTestHost(t, Config{
		TaskQueues:       []string{"goobers-engine"},
		BuildVersion:     "v2",
		Versioning:       true,
		VersionRetention: workerrollout.DefaultVersionLimit,
		RolloutNamespace: "goobers-system",
	}, &fakeFleet{})
	for failures, want := range []time.Duration{
		5 * time.Second,
		10 * time.Second,
		20 * time.Second,
		40 * time.Second,
		80 * time.Second,
		160 * time.Second,
		5 * time.Minute,
		5 * time.Minute,
	} {
		if got := h.nextRoutingCheckInterval(failures + 1); got != want {
			t.Fatalf("%d describe failure(s): interval = %s, want %s", failures+1, got, want)
		}
	}
}

func TestWorkerLivenessIgnoresTemporalOutage(t *testing.T) {
	h := newTestHost(t, Config{
		TaskQueues:   []string{"goobers-engine"},
		BuildVersion: "v2",
		Versioning:   true,
	}, &fakeFleet{})
	h.pollersStarted.Store(true)
	h.describeRouting = func(context.Context, client.Client) (workerrouting.Routing, error) {
		return workerrouting.Routing{}, errors.New("temporal unavailable")
	}
	h.checkRoutingOnce(context.Background(), nil, "")

	handler := h.healthHandler()
	liveness := httptest.NewRecorder()
	handler.ServeHTTP(liveness, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if liveness.Code != http.StatusOK {
		t.Fatalf("GET /healthz = %d, want %d during Temporal outage", liveness.Code, http.StatusOK)
	}
	readiness := httptest.NewRecorder()
	handler.ServeHTTP(readiness, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if readiness.Code != http.StatusServiceUnavailable {
		t.Fatalf("GET /readyz = %d, want %d when version routing cannot be verified", readiness.Code, http.StatusServiceUnavailable)
	}
}
