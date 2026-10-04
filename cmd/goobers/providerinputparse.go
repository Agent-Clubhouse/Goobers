package main

import (
	"errors"
	"strconv"
	"time"
)

// providerInputErrorText renders a caller-owned diagnostic for a provider
// input that failed to parse (parseErr non-nil) or failed the caller's range
// check (parseErr nil). Callers keep their exact historical error text.
type providerInputErrorText func(raw string, parseErr error) string

// The parse helpers below take the already-read raw value, never the input
// name: every provider input must still be read through a literal
// providerInput("<field>", ...) call at the call site, because
// internal/providerstage's source audit (TestProviderInputConsumersMatchSchemas)
// discovers command inputs from those literal calls and checks them against
// the command schema. Reading the input inside a helper would hide the field
// from that audit.

func parseIntInput(raw string, validate func(int) bool, errorText providerInputErrorText) (int, error) {
	value, err := strconv.Atoi(raw)
	if err != nil || !validate(value) {
		return 0, errors.New(errorText(raw, err))
	}
	return value, nil
}

func parseFloatInput(raw string, validate func(float64) bool, errorText providerInputErrorText) (float64, error) {
	value, err := strconv.ParseFloat(raw, 64)
	if err != nil || !validate(value) {
		return 0, errors.New(errorText(raw, err))
	}
	return value, nil
}

func parseDurationInput(raw string, validate func(time.Duration) bool, errorText providerInputErrorText) (time.Duration, error) {
	value, err := time.ParseDuration(raw)
	if err != nil || !validate(value) {
		return 0, errors.New(errorText(raw, err))
	}
	return value, nil
}

// parseBoolInput accepts what strconv.ParseBool accepts, narrowed by
// validate, which sees the raw spelling so a caller can insist on exactly
// "true"/"false".
func parseBoolInput(raw string, validate func(string, bool) bool, errorText providerInputErrorText) (bool, error) {
	value, err := strconv.ParseBool(raw)
	if err != nil || !validate(raw, value) {
		return false, errors.New(errorText(raw, err))
	}
	return value, nil
}
