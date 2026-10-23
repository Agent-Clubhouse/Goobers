package engine

import (
	"errors"

	"go.temporal.io/sdk/temporal"
)

func gateDecisionRefusalCode(err error) (code, message string, ok bool) {
	var appErr *temporal.ApplicationError
	if !errors.As(err, &appErr) {
		return "", "", false
	}
	return appErr.Type(), appErr.Message(), true
}
