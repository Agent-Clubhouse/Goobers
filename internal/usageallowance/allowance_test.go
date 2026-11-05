package usageallowance

import (
	"encoding/json"
	"math"
	"testing"
)

func TestAllowanceDistinguishesUnlimitedAndExhausted(t *testing.T) {
	zeroTokens, zeroCost := int64(0), float64(0)
	exhausted := Allowance{MaxTokens: &zeroTokens, MaxCostUSD: &zeroCost}
	data, err := json.Marshal(exhausted)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != `{"maxTokens":0,"maxCostUSD":0}` {
		t.Fatalf("exhausted JSON = %s", data)
	}
	unlimited, err := json.Marshal(Allowance{})
	if err != nil {
		t.Fatal(err)
	}
	if string(unlimited) != `{}` {
		t.Fatalf("unlimited JSON = %s", unlimited)
	}
}

func TestAllowanceValidateAndClone(t *testing.T) {
	tokens, cost := int64(7), 0.25
	input := &Allowance{MaxTokens: &tokens, MaxCostUSD: &cost}
	cloned := Clone(input)
	tokens, cost = 99, 99
	if *cloned.MaxTokens != 7 || *cloned.MaxCostUSD != 0.25 {
		t.Fatalf("clone changed with source: %+v", cloned)
	}
	negative, nan := int64(-1), math.NaN()
	if err := (Allowance{MaxTokens: &negative}).Validate(); err == nil {
		t.Fatal("negative token allowance accepted")
	}
	if err := (Allowance{MaxCostUSD: &nan}).Validate(); err == nil {
		t.Fatal("NaN cost allowance accepted")
	}
}
