package providers

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
)

// Stable codes for Azure CLI credential failures. They surface as the
// ErrorCode of the failure's journal cause, so status, journals and telemetry
// can tell an expired login from an offline host without any CLI text.
const (
	azureCLIFailureCode            = "azure_cli_failed"
	azureCLITimeoutCode            = "azure_cli_timeout"
	azureCLICanceledCode           = "azure_cli_canceled"
	azureCLINotFoundCode           = "azure_cli_not_found"
	azureCLIStartFailedCode        = "azure_cli_start_failed"
	azureCLIExitCode               = "azure_cli_exit"
	azureCLIPathAmbiguousCode      = "azure_cli_path_ambiguous"
	azureCLISignInRequiredCode     = "azure_cli_sign_in_required"
	azureCLINoAccountCode          = "azure_cli_no_account"
	azureCLINetworkUnreachableCode = "azure_cli_network_unreachable"
)

type azureCLICommandFailure struct {
	code    string
	message string
	cause   error
}

func (e *azureCLICommandFailure) Error() string     { return e.message }
func (e *azureCLICommandFailure) Unwrap() error     { return e.cause }
func (e *azureCLICommandFailure) ErrorCode() string { return e.code }

// azureCLIOutputClass is a fixed classification of failed `az` output. Only
// the class is ever rendered; the output itself never leaves this file.
type azureCLIOutputClass struct {
	code   string
	detail string
	hint   string
}

var (
	azureCLISignInRequired = azureCLIOutputClass{
		code:   azureCLISignInRequiredCode,
		detail: "Azure CLI sign-in expired or requires interaction",
		hint: "run az login (with the configured --tenant, if any) in the same user/process context as Goobers," +
			" then retry",
	}
	azureCLINoAccount = azureCLIOutputClass{
		code:   azureCLINoAccountCode,
		detail: "no Azure CLI account is signed in",
		hint:   "run az login (with the configured --tenant, if any) in the same user/process context as Goobers, then retry",
	}
	azureCLINetworkUnreachable = azureCLIOutputClass{
		code:   azureCLINetworkUnreachableCode,
		detail: "Azure CLI could not reach the network",
		hint: "check network connectivity, DNS, VPN and proxy settings for the Goobers host (for example after" +
			" sleep or a network change), then retry; signing in again does not fix this",
	}
)

// Fixed, lowercase markers. Server-issued sign-in errors prove the network
// worked, so they win over network markers; network markers win over the
// generic `az login` advice the CLI appends to many failures.
var (
	azureCLIServerSignInMarkers = [][]byte{
		[]byte("aadsts700082"), // refresh token expired due to inactivity
		[]byte("aadsts70043"),  // refresh token expired (sign-in frequency policy)
		[]byte("aadsts50173"),  // grant expired after a credential change
		[]byte("aadsts50076"),  // MFA required
		[]byte("aadsts50078"),  // MFA claim expired
		[]byte("aadsts50079"),  // MFA enrollment required
		[]byte("aadsts50158"),  // external security challenge
		[]byte("aadsts530003"), // device must be managed
		[]byte("refresh token has expired"),
		[]byte("interaction_required"),
		[]byte("invalid_grant"),
	}
	azureCLINetworkMarkers = [][]byte{
		[]byte("failed to establish a new connection"),
		[]byte("max retries exceeded"),
		[]byte("network is unreachable"),
		[]byte("unreachable network"),
		[]byte("getaddrinfo"),
		[]byte("nodename nor servname"),
		[]byte("name or service not known"),
		[]byte("name resolution"), // "temporary failure in name resolution"
		[]byte("no route to host"),
		[]byte("connecttimeouterror"),
	}
	azureCLINoAccountMarkers = [][]byte{
		[]byte("to setup account"),
		[]byte("no subscription found"),
	}
	azureCLISignInMarkers = [][]byte{
		[]byte("az login"),
	}
)

// classifyAzureCLIOutput maps failed `az` output to a fixed class by marker
// presence only. It never returns, logs or retains any part of out.
func classifyAzureCLIOutput(out []byte) (azureCLIOutputClass, bool) {
	if len(out) == 0 {
		return azureCLIOutputClass{}, false
	}
	lower := bytes.ToLower(out)
	for _, rule := range []struct {
		markers [][]byte
		class   azureCLIOutputClass
	}{
		{azureCLIServerSignInMarkers, azureCLISignInRequired},
		{azureCLINetworkMarkers, azureCLINetworkUnreachable},
		{azureCLINoAccountMarkers, azureCLINoAccount},
		{azureCLISignInMarkers, azureCLISignInRequired},
	} {
		for _, marker := range rule.markers {
			if bytes.Contains(lower, marker) {
				return rule.class, true
			}
		}
	}
	return azureCLIOutputClass{}, false
}

// Failed token commands can return partial credentials in output or runner
// errors. Classify only typed causes and fixed output markers; never render
// their text or CLI output.
func azureCLICommandError(ctx context.Context, err error, out []byte) error {
	cause := err
	if ctxErr := ctx.Err(); ctxErr != nil {
		cause = errors.Join(err, ctxErr)
	}
	code := azureCLIFailureCode
	detail := "command failed"
	hint := "run az account get-access-token --resource " + AzureDevOpsResourceID +
		" --query expiresOn --output tsv in the same user/process context as Goobers" +
		" (include the configured --tenant, if any); use az login only if that check requests sign-in"
	var pathAmbiguity *azureCLIPathAmbiguityError
	hasPathAmbiguity := errors.As(cause, &pathAmbiguity)

	var lookup *exec.Error
	var start *os.PathError
	var exit *exec.ExitError
	switch {
	case errors.Is(cause, context.DeadlineExceeded):
		code = azureCLITimeoutCode
		detail = "command timed out"
		hint = "check Azure CLI responsiveness and network access in the same user/process context as Goobers"
	case errors.Is(cause, context.Canceled):
		code = azureCLICanceledCode
		detail = "command was canceled"
		hint = "retry the operation if cancellation was unintended"
	case errors.As(cause, &lookup):
		code = azureCLINotFoundCode
		detail = "executable lookup failed"
		hint = "ensure a trusted Azure CLI installation is on PATH in the same user/process context as Goobers"
	case errors.As(cause, &start) && start.Op == "fork/exec":
		// os.StartProcess reports "fork/exec" on every platform, Windows
		// included.
		code = azureCLIStartFailedCode
		detail = "process could not start"
		hint = "check the Azure CLI installation, launcher and executable permissions in the same user/process context as Goobers"
	case errors.As(cause, &exit):
		code = azureCLIExitCode
		status := "process exited unsuccessfully"
		if exitCode := exit.ExitCode(); exitCode >= 0 {
			status = fmt.Sprintf("process exited with code %d", exitCode)
		}
		detail = status
		if class, ok := classifyAzureCLIOutput(out); ok {
			code, detail, hint = class.code, class.detail+" ("+status+")", class.hint
		}
	}
	if hasPathAmbiguity {
		// Timeout and cancellation keep their cause codes; the launcher
		// ambiguity is reported alongside them. Classified output causes are
		// left undistracted.
		appendAmbiguity := true
		switch code {
		case azureCLIFailureCode, azureCLIStartFailedCode, azureCLIExitCode:
			code = azureCLIPathAmbiguousCode
		case azureCLITimeoutCode, azureCLICanceledCode:
		default:
			appendAmbiguity = false
		}
		if appendAmbiguity {
			hint += fmt.Sprintf(
				"; Goobers found %d Azure CLI launchers on PATH and used the first; inspect PATH and remove or reorder stale installations",
				pathAmbiguity.candidateCount,
			)
		}
	}
	return &azureCLICommandFailure{
		code:    code,
		message: "azure CLI get-access-token: " + detail + "; " + hint,
		cause:   cause,
	}
}
