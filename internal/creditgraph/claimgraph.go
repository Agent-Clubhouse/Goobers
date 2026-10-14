package creditgraph

import (
	"fmt"
	"strconv"

	"github.com/goobers/goobers/internal/journal"
)

// claimLedger is what the journal had recorded up to one claim batch. Claims
// are evaluated in journal order against it, so a claim can only be verified
// by records that precede it.
type claimLedger struct {
	seqs          map[uint64]bool
	stageAttempts map[string][]int
	started       map[string]bool
	agents        map[string]bool
	artifacts     map[string]string
	evaluated     int
}

func (l *claimLedger) observe(event journal.Event) {
	l.seqs[event.Seq] = true
	switch event.Type {
	case journal.EventStageStarted:
		if event.Stage == "" {
			return
		}
		key := stageAttemptKey(event.Stage, max(event.Attempt, 1))
		if !l.started[key] {
			l.started[key] = true
			l.stageAttempts[event.Stage] = append(l.stageAttempts[event.Stage], max(event.Attempt, 1))
		}
	case journal.EventAgentLifecycle:
		if event.Agent != nil && event.Agent.ID != "" {
			l.agents[event.Agent.ID] = true
		}
	case journal.EventArtifactRecorded:
		if event.Ref == nil || event.Ref.Digest == "" {
			return
		}
		if _, seen := l.artifacts[event.Ref.Digest]; !seen {
			producer := ""
			if event.Stage != "" {
				producer = stageAttemptKey(event.Stage, max(event.Attempt, 1))
			}
			l.artifacts[event.Ref.Digest] = producer
		}
	}
}

// addClaims evaluates every journaled claim batch. It never adds nodes or
// edges: verified claims are listed in Graph.Claims between recorded nodes,
// and everything else lands in Graph.UnverifiedClaims with its reasons.
func (b *builder) addClaims(events []journal.Event) {
	ledger := &claimLedger{
		seqs: map[uint64]bool{}, stageAttempts: map[string][]int{}, started: map[string]bool{},
		agents: map[string]bool{}, artifacts: map[string]string{},
	}
	for _, event := range events {
		if event.Type == journal.EventRunnerAnnotation && event.Runner != nil {
			if kind, _ := event.Runner[SpanProvenanceKeyKind].(string); kind == ClaimsAnnotation {
				b.addClaimBatch(ledger, event)
			}
		}
		ledger.observe(event)
	}
}

func (b *builder) addClaimBatch(ledger *claimLedger, event journal.Event) {
	claims, writerAgent, dropped := decodeClaimsAnnotation(event.Runner)
	b.graph.DroppedClaims += dropped
	writer := ClaimWriter{Stage: event.Stage, Attempt: max(event.Attempt, 1), Agent: writerAgent}
	for index, claim := range claims {
		if ledger.evaluated >= MaxClaimsPerRun {
			b.graph.DroppedClaims++
			continue
		}
		ledger.evaluated++
		record := ClaimRecord{
			ID:     "claim:" + strconv.FormatUint(event.Seq, 10) + ":" + strconv.Itoa(index),
			Seq:    event.Seq,
			Writer: writer,
			Claim:  claim,
		}
		b.verifyClaim(ledger, &record)
		if len(record.Reasons) == 0 {
			record.Status = ClaimVerified
			b.graph.Claims = append(b.graph.Claims, record)
			continue
		}
		record.Status = ClaimUnverified
		record.WriterNodeID, record.TargetNodeID = "", ""
		b.graph.UnverifiedClaims = append(b.graph.UnverifiedClaims, record)
	}
}

func (b *builder) verifyClaim(ledger *claimLedger, record *ClaimRecord) {
	writerKey := b.verifyClaimWriter(ledger, record)
	targetKey, producer := b.verifyClaimTarget(ledger, record)
	if record.Kind != ClaimAssignment && writerKey != "" {
		target := record.Target
		if (target.Stage != "" && targetKey == writerKey) ||
			(target.Agent != "" && target.Agent == record.Writer.Agent) ||
			(target.Artifact != "" && producer == writerKey) {
			record.reject("claim targets the writer's own work")
		}
	}
	if (record.Kind == ClaimCredit || record.Kind == ClaimBlame) && len(record.Evidence) == 0 {
		record.reject("credit and blame claims need at least one evidence link")
	}
	for _, link := range record.Evidence {
		if link.Artifact != "" {
			if _, recorded := ledger.artifacts[link.Artifact]; !recorded {
				record.reject("evidence artifact " + link.Artifact + " was not recorded before the claim")
			}
			continue
		}
		if link.Seq >= record.Seq || !ledger.seqs[link.Seq] {
			record.reject(fmt.Sprintf("evidence seq %d is not a journal event before the claim", link.Seq))
		}
	}
}

func (b *builder) verifyClaimWriter(ledger *claimLedger, record *ClaimRecord) string {
	if record.Writer.Stage == "" {
		record.reject("claim batch names no writing stage")
		return ""
	}
	key := stageAttemptKey(record.Writer.Stage, record.Writer.Attempt)
	if !ledger.started[key] || !b.recordedNode("stage:"+key) {
		record.reject("writing stage attempt recorded no stage.started before the claim")
		return ""
	}
	record.WriterNodeID = "stage:" + key
	if record.Writer.Agent != "" && !ledger.agents[record.Writer.Agent] {
		record.reject("writer agent recorded no lifecycle before the claim")
	}
	return key
}

// verifyClaimTarget resolves the target to a recorded node and returns its
// stage-attempt key (stage targets) or producing stage-attempt key (artifact
// targets) for the self-reference check.
func (b *builder) verifyClaimTarget(ledger *claimLedger, record *ClaimRecord) (string, string) {
	target := record.Target
	var key, producer, nodeID string
	switch {
	case target.Stage != "":
		attempt := target.Attempt
		if attempt == 0 {
			if attempts := ledger.stageAttempts[target.Stage]; len(attempts) > 0 {
				attempt = attempts[len(attempts)-1]
			}
		}
		key = stageAttemptKey(target.Stage, attempt)
		if attempt == 0 || !ledger.started[key] {
			record.reject("target stage attempt recorded no stage.started before the claim")
			return key, ""
		}
		nodeID = "stage:" + key
	case target.Agent != "":
		if !ledger.agents[target.Agent] {
			record.reject("target agent recorded no lifecycle before the claim")
			return "", ""
		}
		nodeID = "subagent:" + target.Agent
	default:
		var recorded bool
		producer, recorded = ledger.artifacts[target.Artifact]
		if !recorded {
			record.reject("target artifact was not recorded before the claim")
			return "", ""
		}
		nodeID = "evidence:" + target.Artifact
	}
	if !b.recordedNode(nodeID) {
		record.reject("target has no recorded graph node")
		return key, producer
	}
	record.TargetNodeID = nodeID
	return key, producer
}

func (b *builder) recordedNode(id string) bool {
	node, ok := b.graph.Node(id)
	return ok && node.Provenance == ProvenanceRecorded
}

func (r *ClaimRecord) reject(reason string) {
	r.Reasons = append(r.Reasons, reason)
}

// ClaimEvidencePrefix marks a cause-finding evidence entry that cites a
// verified blame claim. Like every non-intervention entry it is
// correlational: a claim is the writer's statement, cross-checked only for
// matching provenance.
const ClaimEvidencePrefix = "agent claim "

// attachClaims copies the graph's claim evaluation into the attribution and
// cites each verified blame claim on the cause findings for the node or stage
// attempt it targets. Unverified claims are listed but never cited.
func attachClaims(graph *Graph, attribution *Attribution) {
	attribution.Claims = graph.Claims
	attribution.UnverifiedClaims = graph.UnverifiedClaims
	attribution.DroppedClaims = graph.DroppedClaims
	for index := range attribution.Causes {
		cause := &attribution.Causes[index]
		stageID := ""
		if node, ok := graph.Node(cause.NodeID); ok && node.Stage != "" {
			stageID = "stage:" + stageAttemptKey(node.Stage, node.Attempt)
		}
		for _, claim := range graph.Claims {
			if claim.Kind != ClaimBlame || (claim.TargetNodeID != cause.NodeID && claim.TargetNodeID != stageID) {
				continue
			}
			cause.Evidence = append(cause.Evidence, fmt.Sprintf("%s%s blame by stage %s attempt %d (confidence %.2f)",
				ClaimEvidencePrefix, claim.ID, claim.Writer.Stage, claim.Writer.Attempt, claim.Confidence))
		}
	}
}
