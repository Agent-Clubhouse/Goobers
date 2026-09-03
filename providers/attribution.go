package providers

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/goobers/goobers/internal/version"
)

// AttributionMarkerPrefix opens the versioned HTML comment marker that
// carries base64-encoded attribution payloads in provider-authored bodies.
const AttributionMarkerPrefix = "<!-- goobers:attribution v1 "

var attributionMarkerStartPattern = regexp.MustCompile(`<!--\s*goobers:attribution\b`)
var attributionMarkerPattern = regexp.MustCompile(`(?s)\n*<!--\s*goobers:attribution\b.*?-->\n?(?:Posted by \*\*Goobers\*\*[^\n]*\n?)?`)
var attributionPayloadPattern = regexp.MustCompile(`<!-- goobers:attribution v1 ([A-Za-z0-9+/=]+) -->`)

type attributionContextKey struct{}

// Attribution identifies the run context responsible for a provider write.
// Values are encoded before entering the HTML comment so untrusted names can
// never terminate or corrupt the marker.
type Attribution struct {
	Schema   int    `json:"schema"`
	Goobers  bool   `json:"goobers"`
	Instance string `json:"instance"`
	Gaggle   string `json:"gaggle"`
	Workflow string `json:"workflow"`
	Task     string `json:"task"`
	Goober   string `json:"goober"`
	Run      string `json:"run"`
	Action   string `json:"action"`
}

// AttributionConfigurer is implemented by providers that can stamp authored
// comments and issue bodies with run attribution.
type AttributionConfigurer interface {
	SetAttribution(Attribution)
}

// WithAttributionContext carries run attribution to daemon-side provider writes.
func WithAttributionContext(ctx context.Context, attribution Attribution) context.Context {
	return context.WithValue(ctx, attributionContextKey{}, attribution)
}

// AttributionFromContext returns run attribution carried by ctx.
func AttributionFromContext(ctx context.Context) (Attribution, bool) {
	attribution, ok := ctx.Value(attributionContextKey{}).(Attribution)
	return attribution, ok
}

// SetAttribution stamps subsequent GitHub writes with the given run attribution.
func (p *GitHubProvider) SetAttribution(attribution Attribution) {
	p.attribution = attribution
}

// SetAttribution stamps subsequent Gitea writes with the given run attribution.
func (p *GiteaProvider) SetAttribution(attribution Attribution) {
	p.attribution = attribution
}

// SetAttribution stamps subsequent ADO writes with the given run attribution.
func (p *ADOProvider) SetAttribution(attribution Attribution) {
	p.attribution = attribution
}

func withAttribution(body string, attribution Attribution, action string) (string, error) {
	if attribution == (Attribution{}) {
		return body, nil
	}
	attribution.Schema = 1
	attribution.Goobers = true
	attribution.Action = action
	if err := validateAttribution(attribution); err != nil {
		return "", err
	}
	data, err := json.Marshal(attribution)
	if err != nil {
		return "", fmt.Errorf("marshal comment attribution: %w", err)
	}
	marker := AttributionMarkerPrefix + base64.StdEncoding.EncodeToString(data) + " -->"
	visible := fmt.Sprintf(
		"Posted by **Goobers** | `%s/%s` | task `%s` | goober `%s` | run `%s` | version `%s`",
		markdownCode(attribution.Gaggle), markdownCode(attribution.Workflow),
		markdownCode(attribution.Task), markdownCode(attribution.Goober),
		markdownCode(shortRunID(attribution.Run)),
		markdownCode(version.Version),
	)
	if attribution.Instance != "" {
		visible += " | instance `" + markdownCode(attribution.Instance) + "`"
	}
	body = strings.TrimSpace(attributionMarkerPattern.ReplaceAllString(body, ""))
	if attributionMarkerStartPattern.MatchString(body) {
		return "", fmt.Errorf("existing comment attribution marker is malformed")
	}
	if body == "" {
		return marker + "\n" + visible, nil
	}
	return body + "\n\n" + marker + "\n" + visible, nil
}

// ParseAttribution decodes the versioned attribution marker in body.
func ParseAttribution(body string) (Attribution, bool, error) {
	starts := attributionMarkerStartPattern.FindAllStringIndex(body, -1)
	if len(starts) == 0 {
		return Attribution{}, false, nil
	}
	if len(starts) != 1 {
		return Attribution{}, false, fmt.Errorf("comment contains %d attribution markers; exactly one is required", len(starts))
	}
	match := attributionPayloadPattern.FindStringSubmatch(body)
	if match == nil {
		return Attribution{}, false, fmt.Errorf("comment attribution marker is malformed or uses an unsupported version")
	}
	data, err := base64.StdEncoding.DecodeString(match[1])
	if err != nil {
		return Attribution{}, false, fmt.Errorf("decode comment attribution: %w", err)
	}
	var attribution Attribution
	if err := json.Unmarshal(data, &attribution); err != nil {
		return Attribution{}, false, fmt.Errorf("parse comment attribution: %w", err)
	}
	if err := validateAttribution(attribution); err != nil {
		return Attribution{}, false, err
	}
	return attribution, true, nil
}

func validateAttribution(attribution Attribution) error {
	if attribution.Schema != 1 {
		return fmt.Errorf("comment attribution schema is %d, want 1", attribution.Schema)
	}
	if !attribution.Goobers {
		return fmt.Errorf("comment attribution goobers flag must be true")
	}
	required := []struct {
		name  string
		value string
	}{
		{"gaggle", attribution.Gaggle},
		{"workflow", attribution.Workflow},
		{"task", attribution.Task},
		{"goober", attribution.Goober},
		{"run", attribution.Run},
		{"action", attribution.Action},
	}
	for _, field := range required {
		if strings.TrimSpace(field.value) == "" {
			return fmt.Errorf("comment attribution %s is required", field.name)
		}
	}
	for name, value := range map[string]string{
		"instance": attribution.Instance,
		"gaggle":   attribution.Gaggle,
		"workflow": attribution.Workflow,
		"task":     attribution.Task,
		"goober":   attribution.Goober,
		"run":      attribution.Run,
		"action":   attribution.Action,
	} {
		if !utf8.ValidString(value) || strings.ContainsAny(value, "\r\n\x00") {
			return fmt.Errorf("comment attribution %s contains invalid control text", name)
		}
		if utf8.RuneCountInString(value) > 256 {
			return fmt.Errorf("comment attribution %s exceeds 256 characters", name)
		}
	}
	return nil
}

func markdownCode(value string) string {
	return strings.ReplaceAll(value, "`", "'")
}

func shortRunID(runID string) string {
	const displayed = 8
	if len(runID) <= displayed {
		return runID
	}
	return runID[:displayed]
}
