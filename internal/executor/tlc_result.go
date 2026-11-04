package executor

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/creditgraph"
)

var tlcMessageStart = regexp.MustCompile(`@!@!@STARTMSG ([0-9]+):([0-9]+) @!@!@\r?\n`)
var tlcInitialStates = regexp.MustCompile(`^Finished computing initial states: [1-9][0-9,]* distinct states? generated`)

type tlcMessage struct {
	code, severity int
	body           string
}

func parseTLCMessages(data []byte) ([]tlcMessage, bool) {
	text := string(data)
	var messages []tlcMessage
	for {
		start := tlcMessageStart.FindStringSubmatchIndex(text)
		if start == nil {
			return messages, !strings.Contains(text, "@!@!@")
		}
		if strings.Contains(text[:start[0]], "@!@!@") {
			return nil, false
		}
		code, _ := strconv.Atoi(text[start[2]:start[3]])
		severity, _ := strconv.Atoi(text[start[4]:start[5]])
		endMarker := fmt.Sprintf("@!@!@ENDMSG %d @!@!@", code)
		body := text[start[1]:]
		end := strings.Index(body, endMarker)
		if end < 0 || strings.Contains(body[:end], "@!@!@") {
			return nil, false
		}
		messages = append(messages, tlcMessage{code: code, severity: severity, body: body[:end]})
		text = body[end+len(endMarker):]
	}
}

func classifyTLC(stdout, stderr []byte, result apiv1.ResultEnvelope, invariant string) (creditgraph.VerificationOutcome, string) {
	inconclusive := creditgraph.VerificationInconclusive
	if result.Outputs[StdoutTruncatedOutput] == true || result.Outputs[StderrTruncatedOutput] == true {
		return inconclusive, "checker output was truncated"
	}
	if result.Status != apiv1.ResultSuccess && (result.Error == nil || result.Error.Code != "nonzero_exit") {
		return inconclusive, "checker did not complete normally"
	}
	if !benignTLCStderr(stderr) {
		return inconclusive, "checker wrote unexpected stderr"
	}
	messages, complete := parseTLCMessages(stdout)
	if !complete {
		return inconclusive, "checker message stream was incomplete"
	}
	seen := map[int]bool{}
	violation := false
	for _, message := range messages {
		seen[message.code] = true
		if message.code == 2107 || message.code == 2110 {
			violation = strings.Contains(message.body, "Invariant "+invariant+" is violated")
			if !violation {
				return inconclusive, "checker violation did not name the requested invariant"
			}
		} else if message.severity == 2 || message.severity == 1 && (message.code != 2121 || !violation) {
			return inconclusive, "checker reported an error other than a checked invariant violation"
		}
	}
	if !seen[2187] || !seen[2185] || !seen[2186] || seen[2188] {
		return inconclusive, "checker did not finish exhaustive finite-model checking"
	}
	if violation && (seen[2216] || seen[2217]) && !seen[2193] && result.Metrics["exitCode"] == 12 {
		return creditgraph.VerificationCounterexample, ""
	}
	if result.Status == apiv1.ResultSuccess && result.Metrics["exitCode"] == 0 && seen[2193] && seen[2199] && !violation && tlcNonemptyInitialStates(messages) && tlcEmptyQueue(messages) {
		return creditgraph.VerificationProven, ""
	}

	return inconclusive, "checker did not establish completed exploration or a concrete counterexample"
}

func benignTLCStderr(data []byte) bool {
	for _, line := range strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n") {
		switch strings.TrimSpace(line) {
		case "", "NOTE: Picked up JDK_JAVA_OPTIONS:", "Picked up JAVA_TOOL_OPTIONS:", "Picked up _JAVA_OPTIONS:":
		default:
			return false
		}
	}
	return true
}

func tlcEmptyQueue(messages []tlcMessage) bool {
	for _, message := range messages {
		if message.code == 2199 && strings.HasSuffix(strings.TrimSpace(message.body), ", 0 states left on queue.") {
			return true
		}
	}
	return false
}

func tlcNonemptyInitialStates(messages []tlcMessage) bool {
	for _, message := range messages {
		if message.code == 2190 && tlcInitialStates.MatchString(strings.TrimSpace(message.body)) {
			return true
		}
	}
	return false
}
