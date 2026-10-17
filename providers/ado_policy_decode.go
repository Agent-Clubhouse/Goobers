package providers

import (
	"encoding/json"
	"fmt"
)

// UnmarshalJSON distinguishes a complete empty policy set from a partial 200
// response. Missing enable/blocking flags must not default to false and turn an
// unproven gate into a successful zero-policy result.
func (r *adoPolicyEvaluationsResponse) UnmarshalJSON(data []byte) error {
	var raw struct {
		Value []json.RawMessage `json:"value"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	if raw.Value == nil {
		return fmt.Errorf("ADO policy response requires an explicit result list")
	}
	r.Value = make([]adoPolicyEvaluation, len(raw.Value))
	for i, data := range raw.Value {
		var shape struct {
			Status        string `json:"status"`
			Configuration *struct {
				IsEnabled  *bool `json:"isEnabled"`
				IsBlocking *bool `json:"isBlocking"`
				Type       *struct {
					ID          string `json:"id"`
					DisplayName string `json:"displayName"`
				} `json:"type"`
			} `json:"configuration"`
		}
		if err := json.Unmarshal(data, &shape); err != nil {
			return err
		}
		cfg := shape.Configuration
		if shape.Status == "" || cfg == nil || cfg.IsEnabled == nil || cfg.IsBlocking == nil || cfg.Type == nil || cfg.Type.ID == "" && cfg.Type.DisplayName == "" {
			return fmt.Errorf("ADO policy result %d is incomplete", i)
		}
		if err := json.Unmarshal(data, &r.Value[i]); err != nil {
			return err
		}
	}
	return nil
}
