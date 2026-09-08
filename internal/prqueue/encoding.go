package prqueue

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v5"

	"github.com/goobers/goobers/api/schemas"
)

// MaxReportBytes bounds the serialized observation independently of item count.
const MaxReportBytes = 4 << 20

type reportWire Report

var compiledReportSchema = sync.OnceValues(func() (*jsonschema.Schema, error) {
	data, err := schemas.FS.ReadFile(schemas.PRQueueEligibility)
	if err != nil {
		return nil, err
	}
	c := jsonschema.NewCompiler()
	c.Draft = jsonschema.Draft2020
	uri := schemas.BaseURI + schemas.PRQueueEligibility
	if err := c.AddResource(uri, bytes.NewReader(data)); err != nil {
		return nil, err
	}
	return c.Compile(uri)
})

// MarshalJSON applies the same bounds as the read boundary, so producers
// cannot persist an observation that operator surfaces must reject.
func (r Report) MarshalJSON() ([]byte, error) {
	if err := r.validate(); err != nil {
		return nil, err
	}
	data, err := json.Marshal(reportWire(r))
	if len(data) > MaxReportBytes {
		return nil, errors.New("queue report exceeds byte limit")
	}
	return data, err
}

// UnmarshalJSON rejects unknown fields and malformed observations rather than
// letting incomplete historical evidence look like an empty or available queue.
func (r *Report) UnmarshalJSON(data []byte) error {
	if len(data) > MaxReportBytes {
		return errors.New("queue report exceeds byte limit")
	}
	schema, err := compiledReportSchema()
	if err != nil {
		return err
	}
	var document any
	if err := json.Unmarshal(data, &document); err != nil {
		return err
	}
	if err := schema.Validate(document); err != nil {
		return fmt.Errorf("queue report schema: %w", err)
	}
	var wire reportWire
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&wire); err != nil {
		return err
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return errors.New("queue report contains trailing data")
	}
	result := Report(wire)
	if err := result.validate(); err != nil {
		return err
	}
	*r = result
	return nil
}

func (r Report) validate() error {
	if r.Version != 1 || r.ObservedAt.IsZero() || r.RepositoryKey == "" || r.Workflow == "" || r.RunID == "" {
		return errors.New("queue report requires version 1, observation time, repository, workflow and run identity")
	}
	if len(r.RepositoryKey) > 4096 || len(r.Workflow) > 256 || len(r.Gaggle) > 256 || len(r.RunID) > 256 {
		return errors.New("queue report identity exceeds field limit")
	}
	if r.Items == nil || len(r.Items) > MaxReportItems || r.OmittedItems < 0 || r.MatchingItems < len(r.Items) || r.MatchingItems-len(r.Items) != r.OmittedItems {
		return errors.New("queue report has invalid item counts")
	}
	seen := make(map[int]bool, len(r.Items))
	for _, item := range r.Items {
		if item.Number <= 0 || seen[item.Number] {
			return errors.New("queue report has invalid or duplicate PR identity")
		}
		seen[item.Number] = true
		if item.Eligible != (item.Reason == "") || len(item.Reason) > 256 || item.NextStep == "" || len(item.NextStep) > 2048 {
			return fmt.Errorf("queue report PR %d has invalid eligibility evidence", item.Number)
		}
		if err := validateClaim(item.Claim, r); err != nil {
			return fmt.Errorf("queue report PR %d: %w", item.Number, err)
		}
	}
	return nil
}

func validateClaim(c ClaimObservation, report Report) error {
	if len(c.OwnerRunID) > 256 || c.NextStep == "" || len(c.NextStep) > 2048 {
		return errors.New("claim observation exceeds field limits or lacks next step")
	}
	switch c.State {
	case "unknown", "unclaimed":
		if c.OwnerRunID != "" || c.ExpiresAt != nil {
			return errors.New("unknown or unclaimed observation has an owner")
		}
	case "expired", "held-by-this-run", "held-by-other-run", "held-in-legacy-namespace":
		if c.OwnerRunID == "" || c.ExpiresAt == nil {
			return errors.New("claim observation lacks owner or expiry")
		}
	default:
		return errors.New("unknown claim observation state")
	}
	switch c.Comparison {
	case "unavailable", "no-local-lease-or-provider-label", "local-lease-and-provider-label", "provider-label-without-live-local-lease", "local-lease-without-provider-label":
	default:
		return errors.New("unknown claim comparison")
	}
	expiry := report.ObservedAt
	if c.ExpiresAt != nil {
		expiry = *c.ExpiresAt
	}
	expected := ObserveClaim(c.State != "unknown", c.OwnerRunID, report.RunID, expiry, report.ObservedAt, c.ProviderClaimLabel)
	if c.State == "held-in-legacy-namespace" && report.Gaggle != "" && (expected.State == "held-by-this-run" || expected.State == "held-by-other-run") {
		expected.State = c.State
	}
	if expected.State != c.State || expected.Comparison != c.Comparison {
		return errors.New("claim state contradicts its owner, expiry, label or comparison")
	}
	return nil
}
