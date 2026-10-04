package harness

import (
	"errors"
	"fmt"
)

// HarnessAuthRequiredCode is the stable diagnostic code for a harness that
// cannot authenticate before model execution.
const HarnessAuthRequiredCode = "HARNESS_AUTH_REQUIRED"

// AuthStatus describes the credential state a harness lifecycle probe observed.
type AuthStatus string

const (
	// AuthStatusAuthenticated means the harness accepted its persisted or
	// explicit model credential.
	AuthStatusAuthenticated AuthStatus = "authenticated"
	// AuthStatusSignedOut means the harness ran but needs an interactive or
	// pre-provisioned sign-in before model execution.
	AuthStatusSignedOut AuthStatus = "signed-out"
	// AuthStatusUnknown means the harness state could not be classified.
	AuthStatusUnknown AuthStatus = "unknown"
)

// AuthInfo is safe to print in status, doctor, and diagnostics output. It must
// not contain token values or credential-provider registrations.
type AuthInfo struct {
	Status      AuthStatus
	Executable  string
	Version     string
	Runner      string
	ProfileDir  string
	Remediation string
}

// AuthRequiredError marks a fail-fast authentication refusal that should be
// surfaced with a stable code and an actionable remediation command.
type AuthRequiredError struct {
	Remediation string
	Err         error
}

func (e *AuthRequiredError) Error() string {
	if e == nil {
		return HarnessAuthRequiredCode
	}
	message := HarnessAuthRequiredCode
	if e.Err != nil {
		message += ": " + e.Err.Error()
	}
	if e.Remediation != "" {
		message += fmt.Sprintf(" — run `%s`", e.Remediation)
	}
	return message
}

func (e *AuthRequiredError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// IsHarnessAuthRequired reports whether err carries the stable auth-required
// diagnostic.
func IsHarnessAuthRequired(err error) bool {
	var authErr *AuthRequiredError
	return errors.As(err, &authErr)
}

// AuthEnvironment returns the default-deny subprocess environment used for
// harness lifecycle operations outside a stage request.
func AuthEnvironment(config EnvironmentConfig) []string {
	return baseEnv(config.ExtraAllowlist, config.Unset)
}
