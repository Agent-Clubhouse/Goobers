package main

import (
	"errors"
	"strconv"
	"strings"
	"time"
)

type providerInputErrorText func(raw string, parseErr error) string

func providerInputValue(name, def string, trim bool) string {
	raw := providerInput(name, def)
	if trim {
		return strings.TrimSpace(raw)
	}
	return raw
}

func parseProviderIntInput(name, def string, trim bool, validate func(int) bool, errorText providerInputErrorText) (int, error) {
	raw := providerInputValue(name, def, trim)
	value, err := strconv.Atoi(raw)
	if err != nil || !validate(value) {
		return 0, errors.New(errorText(raw, err))
	}
	return value, nil
}

func parseProviderFloatInput(name, def string, trim bool, validate func(float64) bool, errorText providerInputErrorText) (float64, error) {
	raw := providerInputValue(name, def, trim)
	value, err := strconv.ParseFloat(raw, 64)
	if err != nil || !validate(value) {
		return 0, errors.New(errorText(raw, err))
	}
	return value, nil
}

func parseProviderDurationInput(name, def string, trim bool, validate func(time.Duration) bool, errorText providerInputErrorText) (time.Duration, error) {
	raw := providerInputValue(name, def, trim)
	value, err := time.ParseDuration(raw)
	if err != nil || !validate(value) {
		return 0, errors.New(errorText(raw, err))
	}
	return value, nil
}

func parseProviderBoolInput(name, def string, trim bool, validate func(string, bool) bool, errorText providerInputErrorText) (bool, error) {
	raw := providerInputValue(name, def, trim)
	value, err := strconv.ParseBool(raw)
	if err != nil || !validate(raw, value) {
		return false, errors.New(errorText(raw, err))
	}
	return value, nil
}
