// Package onecollector validates operator profiles and sends bounded opt-in telemetry.
package onecollector

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// MaxProfileBytes caps a protected JSON profile at one MiB.
const MaxProfileBytes = 1 << 20

// Profile contains operator-owned routing and projections, never built-in destinations.
type Profile struct {
	ProfileVersion     int       `json:"profile_version"`
	EnableEnv          string    `json:"enable_env"`
	Endpoint           string    `json:"endpoint"`
	RoutingKey         KeyRef    `json:"routing_key"`
	InstrumentationKey KeyRef    `json:"instrumentation_key"`
	Mappings           []Mapping `json:"mappings"`
	Inputs             []Input   `json:"inputs,omitempty"`
	Tools              []Tool    `json:"tools,omitempty"`
}

// KeyRef identifies one indirect credential source, never an inline secret.
type KeyRef struct {
	Env      string `json:"env,omitempty"`
	File     string `json:"file,omitempty"`
	Keychain string `json:"keychain,omitempty"`
	Store    string `json:"store,omitempty"`
}

// Mapping selects signal facts for an operator-defined event envelope.
type Mapping struct {
	Signal                string              `json:"signal"`
	Match                 map[string][]string `json:"match,omitempty"`
	EventName             string              `json:"event_name"`
	EnvelopeVersion       string              `json:"envelope_version"`
	Fields                map[string]Field    `json:"fields"`
	Extensions            map[string]Field    `json:"extensions,omitempty"`
	Projections           []Projection        `json:"projections,omitempty"`
	Nonnegative           bool                `json:"nonnegative,omitempty"`
	DataSource            string              `json:"data_source,omitempty"`
	EventNameSource       string              `json:"event_name_source,omitempty"`
	TimestampSource       string              `json:"timestamp_source,omitempty"`
	EnvelopeVersionSource string              `json:"envelope_version_source,omitempty"`
	TimestampFormat       string              `json:"timestamp_format,omitempty"`
}

// Field projects one source fact or one encoded JSON constant.
type Field struct {
	Source   string  `json:"source,omitempty"`
	Constant *string `json:"constant,omitempty"`
}

// Projection rewrites selected attribute keys and optional type companions.
type Projection struct {
	Source      string            `json:"source"`
	Prefix      string            `json:"prefix"`
	KeyEncoding string            `json:"key_encoding"`
	Aliases     map[string]string `json:"aliases,omitempty"`
	TypePrefix  string            `json:"type_prefix,omitempty"`
}

var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// LoadProfile reads a bounded profile file and validates its contract.
func LoadProfile(path string) (*Profile, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, errors.New("onecollector: cannot open profile")
	}
	raw, err := io.ReadAll(io.LimitReader(file, MaxProfileBytes+1))
	closeErr := file.Close()
	if err != nil || closeErr != nil {
		return nil, errors.New("onecollector: cannot read profile")
	}
	return ParseProfile(raw)
}

// ParseProfile decodes strict JSON and validates the operator profile.
func ParseProfile(raw []byte) (*Profile, error) {
	if _, err := DecodeObject(string(raw), MaxProfileBytes); err != nil {
		return nil, err
	}
	var profile Profile
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&profile); err != nil {
		return nil, errors.New("onecollector: invalid profile fields")
	}
	if err := profile.Validate(); err != nil {
		return nil, err
	}
	return &profile, nil
}

// Validate checks profile bounds and declarations without resolving credentials.
func (p Profile) Validate() error {
	fail := func() error { return errors.New("onecollector: invalid profile contract") }
	target, err := url.Parse(p.Endpoint)
	if p.ProfileVersion != 1 || !envName.MatchString(p.EnableEnv) || err != nil ||
		target.Scheme != "https" || target.Host == "" || target.User != nil ||
		target.RawQuery != "" || target.Fragment != "" || !p.RoutingKey.valid() || !p.InstrumentationKey.valid() ||
		len(p.Mappings) == 0 || len(p.Mappings) > 64 {
		return fail()
	}
	for _, mapping := range p.Mappings {
		if err := mapping.validate(); err != nil {
			return err
		}
	}
	return p.validateInputs()
}

func (mapping Mapping) validate() error {
	if mapping.Signal != "journal" && mapping.Signal != "trace" && mapping.Signal != "metric" && mapping.Signal != "input" {
		return errors.New("onecollector: invalid signal")
	}
	if err := mapping.validateEnvelope(); err != nil {
		return err
	}
	if mapping.Signal == "metric" {
		for _, selector := range []string{"metric.name", "metric.unit", "metric.kind"} {
			if len(mapping.Match[selector]) == 0 {
				return errors.New("onecollector: metric mappings require explicit name, unit and kind")
			}
		}
	}
	for _, fields := range []map[string]Field{mapping.Fields, mapping.Extensions} {
		if err := validateFields(fields); err != nil {
			return err
		}
	}
	for selector, values := range mapping.Match {
		if selector == "" || len(values) == 0 || len(values) > 128 {
			return errors.New("onecollector: invalid mapping match")
		}
	}
	for _, projection := range mapping.Projections {
		if (projection.Source != "attributes" && projection.Source != "resource" && projection.Source != "scope") ||
			projection.KeyEncoding != "hex" || projection.Prefix == "" || len(projection.Aliases) > 128 {
			return errors.New("onecollector: invalid attribute projection")
		}
	}
	return nil
}

func (mapping Mapping) validateEnvelope() error {
	if (mapping.EventName == "" && mapping.EventNameSource == "") || len(mapping.EventName) > 256 || (mapping.EnvelopeVersion == "" && mapping.EnvelopeVersionSource == "") ||
		(len(mapping.Fields) == 0 && mapping.DataSource == "") || (len(mapping.Fields) > 0 && mapping.DataSource != "") || len(mapping.Fields) > 256 || len(mapping.Extensions) > 32 ||
		len(mapping.Projections) > 8 || len(mapping.Match) > 32 ||
		mapping.DataSource != "" && len(mapping.Projections) != 0 {
		return errors.New("onecollector: invalid envelope mapping")
	}
	switch mapping.TimestampFormat {
	case "", "unix_nano", "rfc3339", "unix_nano_millis", "rfc3339_millis":
		return nil
	default:
		return errors.New("onecollector: invalid timestamp format")
	}
}

func validateFields(fields map[string]Field) error {
	for key, field := range fields {
		if key == "" || len(key) > 256 || (field.Source == "") == (field.Constant == nil) {
			return errors.New("onecollector: invalid field mapping")
		}
		if field.Constant != nil {
			if _, err := DecodeObject(`{"value":`+*field.Constant+`}`, MaxProfileBytes); err != nil {
				return errors.New("onecollector: invalid constant")
			}
		}
	}
	return nil
}

func (r KeyRef) valid() bool {
	count := 0
	for _, value := range []string{r.Env, r.File, r.Keychain, r.Store} {
		if value != "" {
			count++
		}
	}
	return count == 1
}

// Enabled reports whether the declared environment variable is exactly "1".
func (p Profile) Enabled() bool { return os.Getenv(p.EnableEnv) == "1" }

// HasSignal reports whether a mapping consumes the given signal.
func (p Profile) HasSignal(signal string) bool {
	for _, mapping := range p.Mappings {
		if mapping.Signal == signal {
			return true
		}
	}
	return false
}

// Facts uses JSON tokens so int64 values and raw JSON never pass through float64.
type Facts map[string]json.RawMessage

// JSON encodes a trusted fact without changing numeric tokens or HTML text.
func JSON(value any) json.RawMessage {
	raw, err := compactJSON(value)
	if err != nil {
		panic("onecollector: invalid internal fact")
	}
	return raw
}

// Matches requires every selector to match at least one allowed value.
func (mapping Mapping) Matches(facts Facts) bool {
	for selector, allowed := range mapping.Match {
		raw, found := facts[selector]
		if !found {
			return false
		}
		value := string(raw)
		var text string
		if json.Unmarshal(raw, &text) == nil {
			value = text
		}
		matched := false
		for _, candidate := range allowed {
			matched = matched || candidate == value
		}
		if !matched {
			return false
		}
	}
	return true
}

func projectFields(fields map[string]Field, facts Facts) map[string]any {
	result := make(map[string]any, len(fields))
	for key, field := range fields {
		if field.Constant != nil {
			result[key] = json.RawMessage(*field.Constant)
		} else if value, exists := facts[field.Source]; exists {
			result[key] = value
		}
	}
	return result
}

// Envelope projects facts and formats an event without sending it.
func (mapping Mapping) Envelope(facts Facts, key, timestamp string) (Envelope, error) {
	size := 0
	for _, fields := range []map[string]Field{mapping.Fields, mapping.Extensions} {
		for name, field := range fields {
			size += len(name) + 32
			if field.Constant != nil {
				size += len(*field.Constant)
			} else {
				size += len(facts[field.Source])
			}
			if size > MaxBatchBytes-1024 {
				return Envelope{}, errors.New("onecollector: projected record byte limit")
			}
		}
	}
	var output any
	data := projectFields(mapping.Fields, facts)
	output = data
	if mapping.DataSource != "" {
		raw, found := facts[mapping.DataSource]
		if !found {
			return Envelope{}, errors.New("onecollector: missing data source")
		}
		object, err := DecodeJSON(string(raw), MaxBatchBytes)
		if err != nil {
			return Envelope{}, err
		}
		output = object
	}
	for _, projection := range mapping.Projections {
		source := projection.Source + "."
		for selector, value := range facts {
			if !strings.HasPrefix(selector, source) || strings.HasSuffix(selector, ".otlp_type") {
				continue
			}
			name := strings.TrimPrefix(selector, source)
			key := projection.Prefix + hex.EncodeToString([]byte(name))
			_, complex := facts[selector+".otlp_type"]
			var text string
			isString := json.Unmarshal(value, &text) == nil
			if alias, exists := projection.Aliases[name]; exists && isString && !complex {
				key = alias
			}
			if _, duplicate := data[key]; duplicate {
				return Envelope{}, errors.New("onecollector: projection field collision")
			}
			data[key] = value
			if kind, found := facts[selector+".otlp_type"]; found && projection.TypePrefix != "" {
				typeKey := projection.TypePrefix + key
				if _, duplicate := data[typeKey]; duplicate {
					return Envelope{}, errors.New("onecollector: projection type collision")
				}
				data[typeKey] = kind
			}
		}
	}
	rawFields := func(fields map[string]Field) map[string]bool {
		result := make(map[string]bool)
		for name, field := range fields {
			switch field.Source {
			case "journal.raw_json", "journal.otlp_json", "trace.raw_json":
				result[name] = true
			}
		}
		return result
	}
	text := func(source, fallback string) (string, error) {
		if source == "" {
			return fallback, nil
		}
		var value string
		if json.Unmarshal(facts[source], &value) != nil || value == "" {
			return "", errors.New("onecollector: invalid envelope source")
		}
		return value, nil
	}
	name, err := text(mapping.EventNameSource, mapping.EventName)
	if err != nil {
		return Envelope{}, err
	}
	version, err := text(mapping.EnvelopeVersionSource, mapping.EnvelopeVersion)
	if err != nil {
		return Envelope{}, err
	}
	timestamp, err = text(mapping.TimestampSource, timestamp)
	if err != nil {
		return Envelope{}, err
	}
	if mapping.TimestampFormat == "unix_nano" || mapping.TimestampFormat == "unix_nano_millis" {
		nanos, err := strconv.ParseUint(timestamp, 10, 64)
		if err != nil {
			return Envelope{}, errors.New("onecollector: invalid nanosecond timestamp")
		}
		instant := time.Unix(int64(nanos/1e9), int64(nanos%1e9)).UTC()
		if mapping.TimestampFormat == "unix_nano_millis" {
			timestamp = instant.Format("2006-01-02T15:04:05.000Z")
		} else {
			timestamp = instant.Format(time.RFC3339Nano)
		}
	} else if mapping.TimestampFormat == "rfc3339_millis" {
		instant, err := time.Parse(time.RFC3339Nano, timestamp)
		if err != nil {
			return Envelope{}, errors.New("onecollector: invalid RFC3339 timestamp")
		}
		timestamp = instant.UTC().Format("2006-01-02T15:04:05.000Z")
	} else if mapping.TimestampFormat != "" && mapping.TimestampFormat != "rfc3339" {
		return Envelope{}, errors.New("onecollector: invalid timestamp format")
	}
	return Envelope{Name: name, Time: timestamp, Ver: version, IKey: key,
		Ext: projectFields(mapping.Extensions, facts), Data: output, RawDataFields: rawFields(mapping.Fields), RawExtensionFields: rawFields(mapping.Extensions),
		PreserveInput: mapping.Signal == "input"}, nil
}
