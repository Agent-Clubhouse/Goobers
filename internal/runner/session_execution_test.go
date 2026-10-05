package runner

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/sessioning"
	"github.com/goobers/goobers/internal/workflow"
	"github.com/goobers/goobers/internal/worktree"
)

func sessionRunFixture(t *testing.T) (*Runner, StartInput, journal.RunIdentity, *childOriginGoober) {
	t.Helper()
	root := t.TempDir()
	manager, err := worktree.NewManager(filepath.Join(root, "worktrees"))
	if err != nil {
		t.Fatal(err)
	}
	spec := apiv1.WorkflowSpec{Gaggle: "g", Start: "respond", Triggers: []apiv1.Trigger{{Type: apiv1.TriggerManual}}, Tasks: []apiv1.Task{{Name: "respond", Type: apiv1.TaskAgentic, Goober: "planner", Goal: "Respond using the session context.", Workspace: apiv1.WorkspaceScratch, Capabilities: []string{"agent:model"}, Retry: &apiv1.RetryPolicy{MaxAttempts: 1}}}}
	machine, err := workflow.Compile(workflow.Definition{Name: "interactive-session", DSLVersion: "3.1", Version: 1, Spec: spec})
	if err != nil {
		t.Fatal(err)
	}
	base, err := New(Config{Worktrees: manager, RunsDir: filepath.Join(root, "runs"), ScratchDir: filepath.Join(root, "scratch"), ConfigGeneration: journal.Digest([]byte("config")), RepoCloneURL: func(apiv1.RepoRef) (string, error) { t.Fatal("session touched repository"); return "", nil }})
	if err != nil {
		t.Fatal(err)
	}
	actor := sessioning.Actor{Issuer: "https://identity.example", Subject: "alice"}
	message := sessioning.Message{ID: "message-a", SessionID: "session-a", Sequence: 1, ActorKind: "human", Actor: &actor, Text: "Help scope the backlog.", CreatedAt: time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC), TurnID: "turn-a"}
	runID := strings.Repeat("a", 32)
	start := sessioning.StartEnvelope{Kind: sessioning.StartKind, Gaggle: "g", SessionID: message.SessionID, TurnID: message.TurnID, MessageID: message.ID, MessageDigest: journal.Digest([]byte(message.Text)), AuthorityDigest: journal.Digest([]byte("authority")), Profile: sessioning.Profile{Goober: "planner", ConfigGeneration: base.cfg.ConfigGeneration, GooberDigest: journal.Digest([]byte("goober"))}}
	inputs := sessioning.ExecutionInputs{Version: 1, AcceptanceID: "trigger-" + runID, Start: start, Messages: []sessioning.Message{message}}
	raw, err := inputs.Validate(runID, "g")
	if err != nil {
		t.Fatal(err)
	}
	envelope, _ := json.Marshal(start)
	lineage := journal.SessionLineage{Gaggle: "g", SessionID: message.SessionID, TurnID: message.TurnID, MessageID: message.ID, AcceptanceID: inputs.AcceptanceID, EnvelopeDigest: journal.Digest(envelope), InputDigest: journal.Digest(raw)}
	id := journal.RunIdentity{RunID: runID, Gaggle: "g", Workflow: machine.Def.Name, WorkflowDigest: machine.Digest(), GooberDigest: start.GooberDigest, ConfigGeneration: start.ConfigGeneration, Trigger: journal.Trigger{Kind: journal.TriggerSignal, Ref: "session:session-a:turn-a"}, Session: &lineage}
	in := StartInput{RunID: runID, Gaggle: "g", Machine: machine, GooberDigest: id.GooberDigest, Trigger: id.Trigger, SessionInputs: &inputs}
	return base, in, id, &childOriginGoober{}
}

func TestSessionDriverPublishesRealProvenanceAndBoundedContext(t *testing.T) {
	base, in, id, goober := sessionRunFixture(t)
	driver, err := base.ForSessionExecution(id, func(string, ArtifactRecorder, SecretRegistrar) (invoke.Goober, error) { return goober, nil })
	if err != nil {
		t.Fatal(err)
	}
	published := false
	in.OnJournalPublished = func() error {
		rd, err := journal.OpenReadOnly(filepath.Join(base.cfg.RunsDir, id.RunID))
		if err != nil {
			return err
		}
		actual, err := rd.Identity()
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(actual.Session, id.Session) {
			t.Fatal(actual)
		}
		published = true
		return nil
	}
	result, err := driver.Start(t.Context(), in)
	if err != nil || result.Phase != journal.PhaseCompleted || !published {
		t.Fatal(result, err)
	}
	if len(goober.envelopes) != 1 || len(goober.envelopes[0].ContextPointers) != 1 {
		t.Fatal("context not delivered", goober.envelopes)
	}
	pointer := goober.envelopes[0].ContextPointers[0]
	if pointer.Name != sessioning.ContextInputName || pointer.Integrity != apiv1.IntegrityUnapproved {
		t.Fatal(pointer)
	}
	raw, err := os.ReadFile(filepath.Join(base.cfg.RunsDir, id.RunID, pointer.Artifact.Path))
	if err != nil {
		t.Fatal(err)
	}
	got, err := sessioning.ParseExecutionInputs(raw, id.RunID, id.Gaggle)
	if err != nil || got.Messages[0].Actor.Subject != "alice" {
		t.Fatal(got, err)
	}
	if _, err = base.Resume(t.Context(), ResumeInput{RunID: id.RunID, Machine: in.Machine}); err == nil {
		t.Fatal("automation driver resumed human session")
	}
	if _, err = driver.RerunStage(t.Context(), RerunStageInput{RunID: id.RunID, Machine: in.Machine, Stage: "respond", Actor: "human"}); err == nil {
		t.Fatal("reopened immutable turn without accepted message")
	}
}

func TestSessionDriverRejectsMissingAndChangedAuthorityBeforeJournal(t *testing.T) {
	for _, kind := range []string{"ordinary-driver", "missing-input", "changed-message", "foreign-run"} {
		t.Run(kind, func(t *testing.T) {
			base, in, id, goober := sessionRunFixture(t)
			driver, err := base.ForSessionExecution(id, func(string, ArtifactRecorder, SecretRegistrar) (invoke.Goober, error) { return goober, nil })
			if err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "ordinary-driver":
				driver = base
			case "missing-input":
				in.SessionInputs = nil
			case "changed-message":
				in.SessionInputs.Messages[0].Text = "different"
			case "foreign-run":
				in.RunID = strings.Repeat("b", 32)
			}
			if _, err = driver.Start(t.Context(), in); err == nil {
				t.Fatal("invalid authority accepted")
			}
			if _, err = os.Stat(filepath.Join(base.cfg.RunsDir, in.RunID)); !os.IsNotExist(err) {
				t.Fatal("refusal created run", err)
			}
		})
	}
}
