package creditgraph

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// The run-scoped Backprop claim log (#7119). An agentic stage appends typed
// claims — who owns what, what it relied on, and whom it credits or blames —
// to ClaimsFileName in its GOOBERS_TELEMETRY_DIR. When the stage process
// exits, the harness journals the bounded, sanitized batch as one
// runner.annotation of kind ClaimsAnnotation, attributed by the runner (not
// the agent) to the writing stage attempt and, when unambiguous, its root
// nested agent. The journal is the claim log: it is append-only, durable, and
// replayed in journal order. Claims are evidence, never edges: Build keeps the
// claims whose writer, target, and evidence match recorded provenance and
// reports every other claim separately as unverified.
const (
	// ClaimSchemaVersion identifies one claim's wire shape.
	ClaimSchemaVersion = "goobers.dev/backprop/claim/v1"
	// ClaimsFileName is the JSONL file an agent appends claims to inside its
	// stage-scoped GOOBERS_TELEMETRY_DIR.
	ClaimsFileName = "backprop-claims.jsonl"
	// ClaimsAnnotation is the runner.annotation kind that journals one stage
	// attempt's claim batch.
	ClaimsAnnotation = "backprop-claims"

	// MaxClaimsPerAttempt bounds the claims one stage attempt may record.
	MaxClaimsPerAttempt = 32
	// MaxClaimsPerRun bounds the claims one run contributes to its graph. Claims
	// beyond it, in journal order, are counted as dropped and never evaluated.
	MaxClaimsPerRun = 256
	// MaxClaimStatementBytes bounds a claim's sanitized free-text statement.
	MaxClaimStatementBytes = 1024
	// MaxClaimEvidence bounds the evidence links on one claim.
	MaxClaimEvidence = 8
	// MaxClaimRefBytes bounds a stage or agent reference.
	MaxClaimRefBytes = 128

	maxClaimLineBytes = 8 << 10
	maxClaimFileBytes = MaxClaimsPerAttempt * maxClaimLineBytes
)

// Annotation payload keys for ClaimsAnnotation.
const (
	// ClaimsKeySchema names the claim wire schema in the annotation payload.
	ClaimsKeySchema = "schema"
	// ClaimsKeyClaims holds the normalized claim batch.
	ClaimsKeyClaims = "claims"
	// ClaimsKeyWriterAgent names the runner-resolved root agent of the writing
	// stage attempt, when it is unambiguous.
	ClaimsKeyWriterAgent = "writerAgent"
	// ClaimsKeyDropped counts lines the harness refused at ingestion.
	ClaimsKeyDropped = "dropped"
)

// ClaimKind is the type of a Backprop claim.
type ClaimKind string

// The claim taxonomy.
const (
	// ClaimAssignment states that the target stage or agent owns the work the
	// statement describes.
	ClaimAssignment ClaimKind = "assignment"
	// ClaimReliance states that the writer consumed the target's output.
	ClaimReliance ClaimKind = "reliance"
	// ClaimCredit states that the target contributed positively to the outcome.
	ClaimCredit ClaimKind = "credit"
	// ClaimBlame states that the target contributed negatively to the outcome.
	ClaimBlame ClaimKind = "blame"
)

// ClaimStatus grades a claim against recorded provenance.
type ClaimStatus string

// Claim statuses.
const (
	// ClaimVerified means the writer, target, and every evidence link resolve
	// to journal records that precede the claim.
	ClaimVerified ClaimStatus = "verified"
	// ClaimUnverified means at least one of them does not. Unverified claims
	// are reported, never projected into the graph's structure or scores.
	ClaimUnverified ClaimStatus = "unverified"
)

// ClaimTarget names exactly one element a claim is about. Attempt narrows a
// stage target; zero means the latest attempt recorded before the claim.
type ClaimTarget struct {
	Stage    string `json:"stage,omitempty"`
	Attempt  int    `json:"attempt,omitempty"`
	Agent    string `json:"agent,omitempty"`
	Artifact string `json:"artifact,omitempty"`
}

// ClaimEvidence links one journal record supporting a claim: either a recorded
// artifact by content digest or a journal event by sequence number.
type ClaimEvidence struct {
	Artifact string `json:"artifact,omitempty"`
	Seq      uint64 `json:"seq,omitempty"`
}

// Claim is one agent-authored statement in the run-scoped claim log.
type Claim struct {
	Kind       ClaimKind       `json:"kind"`
	Target     ClaimTarget     `json:"target"`
	Statement  string          `json:"statement,omitempty"`
	Evidence   []ClaimEvidence `json:"evidence,omitempty"`
	Confidence float64         `json:"confidence"`
}

// ClaimWriter is the runner-attributed author of a claim.
type ClaimWriter struct {
	Stage   string `json:"stage"`
	Attempt int    `json:"attempt"`
	Agent   string `json:"agent,omitempty"`
}

// ClaimRecord is one journaled claim as the graph evaluated it.
type ClaimRecord struct {
	ID           string      `json:"id"`
	Seq          uint64      `json:"seq"`
	Writer       ClaimWriter `json:"writer"`
	WriterNodeID string      `json:"writerNodeId,omitempty"`
	TargetNodeID string      `json:"targetNodeId,omitempty"`
	Status       ClaimStatus `json:"status"`
	Reasons      []string    `json:"reasons,omitempty"`
	Claim
}

var claimDigestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// NormalizeClaim validates an untrusted claim and returns its sanitized form:
// control, format (bidirectional and zero-width), and invalid UTF-8 characters
// are removed from the statement, whitespace runs collapse to one space, and
// every reference is checked against the schema's bounds.
func NormalizeClaim(claim Claim) (Claim, error) {
	switch claim.Kind {
	case ClaimAssignment, ClaimReliance, ClaimCredit, ClaimBlame:
	default:
		return Claim{}, fmt.Errorf("kind must be assignment, reliance, credit, or blame")
	}
	target, err := normalizeClaimTarget(claim.Target)
	if err != nil {
		return Claim{}, err
	}
	if claim.Kind == ClaimAssignment && target.Artifact != "" {
		return Claim{}, errors.New("an assignment targets a stage or agent, not an artifact")
	}
	statement := SanitizeClaimText(claim.Statement)
	if len(statement) > MaxClaimStatementBytes {
		return Claim{}, fmt.Errorf("statement exceeds %d bytes after sanitizing", MaxClaimStatementBytes)
	}
	if claim.Kind == ClaimAssignment && statement == "" {
		return Claim{}, errors.New("an assignment needs a statement describing the owned work")
	}
	if math.IsNaN(claim.Confidence) || claim.Confidence < 0 || claim.Confidence > 1 {
		return Claim{}, errors.New("confidence must be between 0 and 1")
	}
	if len(claim.Evidence) > MaxClaimEvidence {
		return Claim{}, fmt.Errorf("at most %d evidence links are allowed", MaxClaimEvidence)
	}
	var evidence []ClaimEvidence
	for _, link := range claim.Evidence {
		if (link.Artifact == "") == (link.Seq == 0) {
			return Claim{}, errors.New("each evidence link names exactly one artifact digest or journal seq")
		}
		if link.Artifact != "" && !claimDigestPattern.MatchString(link.Artifact) {
			return Claim{}, errors.New("evidence artifact must be a sha256:<64 hex> content digest")
		}
		evidence = append(evidence, link)
	}
	return Claim{Kind: claim.Kind, Target: target, Statement: statement, Evidence: evidence, Confidence: claim.Confidence}, nil
}

func normalizeClaimTarget(target ClaimTarget) (ClaimTarget, error) {
	set := 0
	for _, value := range []string{target.Stage, target.Agent, target.Artifact} {
		if value != "" {
			set++
		}
	}
	if set != 1 {
		return ClaimTarget{}, errors.New("target names exactly one stage, agent, or artifact")
	}
	if target.Attempt < 0 || (target.Attempt != 0 && target.Stage == "") {
		return ClaimTarget{}, errors.New("target attempt must be positive and only narrows a stage")
	}
	if target.Artifact != "" && !claimDigestPattern.MatchString(target.Artifact) {
		return ClaimTarget{}, errors.New("target artifact must be a sha256:<64 hex> content digest")
	}
	for _, ref := range []string{target.Stage, target.Agent} {
		if ref != "" && !validClaimRef(ref) {
			return ClaimTarget{}, fmt.Errorf("target stage or agent must be at most %d printable, non-space bytes", MaxClaimRefBytes)
		}
	}
	return target, nil
}

func validClaimRef(ref string) bool {
	if len(ref) > MaxClaimRefBytes || !utf8.ValidString(ref) {
		return false
	}
	for _, r := range ref {
		if unicode.IsSpace(r) || unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return false
		}
	}
	return true
}

// SanitizeClaimText strips characters that could disguise or restructure
// untrusted agent text when it is later rendered: invalid UTF-8, control
// characters, and Unicode format characters (bidirectional overrides,
// zero-width joiners). Whitespace runs collapse to a single space.
func SanitizeClaimText(text string) string {
	var out strings.Builder
	space := false
	for _, r := range strings.ToValidUTF8(text, "") {
		switch {
		case unicode.IsSpace(r):
			space = out.Len() > 0
		case unicode.IsControl(r), unicode.Is(unicode.Cf, r):
		default:
			if space {
				out.WriteByte(' ')
				space = false
			}
			out.WriteRune(r)
		}
	}
	return out.String()
}

// AppendClaim validates claim and appends it to dir's claim file. It refuses
// a symlinked file and a file that already holds MaxClaimsPerAttempt claims.
func AppendClaim(dir string, claim Claim) (Claim, error) {
	if strings.TrimSpace(dir) == "" {
		return Claim{}, errors.New("no stage telemetry directory (GOOBERS_TELEMETRY_DIR is unset)")
	}
	normalized, err := NormalizeClaim(claim)
	if err != nil {
		return Claim{}, err
	}
	line, err := json.Marshal(normalized)
	if err != nil {
		return Claim{}, fmt.Errorf("encode claim: %w", err)
	}
	if len(line)+1 > maxClaimLineBytes {
		return Claim{}, fmt.Errorf("encoded claim exceeds %d bytes", maxClaimLineBytes)
	}
	path := filepath.Join(dir, ClaimsFileName)
	if info, statErr := os.Lstat(path); statErr == nil && !info.Mode().IsRegular() {
		return Claim{}, fmt.Errorf("%s is not a regular file", path)
	}
	existing, _ := ReadClaimFile(dir)
	if len(existing) >= MaxClaimsPerAttempt {
		return Claim{}, fmt.Errorf("this stage attempt already recorded the maximum of %d claims", MaxClaimsPerAttempt)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return Claim{}, fmt.Errorf("open claim log: %w", err)
	}
	_, writeErr := file.Write(append(line, '\n'))
	if closeErr := file.Close(); writeErr == nil {
		writeErr = closeErr
	}
	if writeErr != nil {
		return Claim{}, fmt.Errorf("append claim: %w", writeErr)
	}
	return normalized, nil
}

// ReadClaimFile reads one stage attempt's claim file. Each valid line yields a
// normalized claim; a malformed, invalid, oversized, or over-budget line is
// counted as dropped. Neither a missing file nor a read error escapes.
func ReadClaimFile(dir string) ([]Claim, int) {
	if dir == "" {
		return nil, 0
	}
	path := filepath.Join(dir, ClaimsFileName)
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil, 0
	}
	if err != nil || !info.Mode().IsRegular() {
		return nil, 1
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, 1
	}
	defer func() { _ = file.Close() }()
	var claims []Claim
	dropped := 0
	if info.Size() > maxClaimFileBytes {
		dropped++
	}
	scanner := bufio.NewScanner(io.LimitReader(file, maxClaimFileBytes))
	scanner.Buffer(make([]byte, 0, 4096), maxClaimLineBytes)
	for scanner.Scan() {
		trimmed := bytes.TrimSpace(scanner.Bytes())
		if len(trimmed) == 0 {
			continue
		}
		claim, ok := decodeClaim(trimmed)
		if !ok || len(claims) >= MaxClaimsPerAttempt {
			dropped++
			continue
		}
		claims = append(claims, claim)
	}
	if scanner.Err() != nil {
		dropped++
	}
	return claims, dropped
}

func decodeClaim(data []byte) (Claim, bool) {
	var claim Claim
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&claim) != nil || decoder.Decode(&struct{}{}) != io.EOF {
		return Claim{}, false
	}
	normalized, err := NormalizeClaim(claim)
	return normalized, err == nil
}

// ClaimsAnnotationPayload builds the runner.annotation payload that journals
// one stage attempt's claims. writerAgent is runner-resolved, never read from
// the agent's own claims.
func ClaimsAnnotationPayload(claims []Claim, dropped int, writerAgent string) map[string]any {
	payload := map[string]any{
		SpanProvenanceKeyKind: ClaimsAnnotation,
		ClaimsKeySchema:       ClaimSchemaVersion,
		ClaimsKeyClaims:       claims,
	}
	if writerAgent != "" {
		payload[ClaimsKeyWriterAgent] = writerAgent
	}
	if dropped > 0 {
		payload[ClaimsKeyDropped] = dropped
	}
	return payload
}

// decodeClaimsAnnotation re-validates a journaled batch: the journal is the
// trust boundary for readers, so entries that fail today's schema are dropped
// rather than trusted because an earlier writer accepted them.
func decodeClaimsAnnotation(runner map[string]any) (claims []Claim, writerAgent string, dropped int) {
	writerAgent, _ = runner[ClaimsKeyWriterAgent].(string)
	if !validClaimRef(writerAgent) {
		writerAgent = ""
	}
	switch value := runner[ClaimsKeyDropped].(type) {
	case float64:
		if value > 0 {
			dropped = int(math.Min(value, maxClaimFileBytes))
		}
	case int:
		dropped = max(0, min(value, maxClaimFileBytes))
	}
	raw, ok := runner[ClaimsKeyClaims].([]any)
	if !ok {
		if typed, typedOK := runner[ClaimsKeyClaims].([]Claim); typedOK {
			for _, claim := range typed {
				raw = append(raw, claim)
			}
		} else if runner[ClaimsKeyClaims] != nil {
			dropped++
		}
	}
	for _, entry := range raw {
		data, err := json.Marshal(entry)
		if err != nil {
			dropped++
			continue
		}
		claim, valid := decodeClaim(data)
		if !valid {
			dropped++
			continue
		}
		claims = append(claims, claim)
	}
	return claims, writerAgent, dropped
}
