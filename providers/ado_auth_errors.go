package providers

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
)

type azureCLICommandFailure struct {
	message string
	cause   error
}

func (e *azureCLICommandFailure) Error() string { return e.message }
func (e *azureCLICommandFailure) Unwrap() error { return e.cause }

// Failed token commands can return partial credentials in output or runner
// errors. Classify only typed causes; never render their text or CLI output.
func azureCLICommandError(ctx context.Context, err error) error {
	cause := err
	if ctxErr := ctx.Err(); ctxErr != nil {
		cause = errors.Join(err, ctxErr)
	}
	detail := "command failed"
	hint := "run az account get-access-token --resource " + AzureDevOpsResourceID +
		" --query expiresOn --output tsv in the same user/process context as Goobers" +
		" (include the configured --tenant, if any); use az login only if that check requests sign-in"

	var lookup *exec.Error
	var start *os.PathError
	var exit *exec.ExitError
	switch {
	case errors.Is(cause, context.DeadlineExceeded):
		detail = "command timed out"
		hint = "check Azure CLI responsiveness and network access in the same user/process context as Goobers"
	case errors.Is(cause, context.Canceled):
		detail = "command was canceled"
		hint = "retry the operation if cancellation was unintended"
	case errors.As(cause, &lookup):
		detail = "executable lookup failed"
		hint = "ensure a trusted Azure CLI installation is on PATH in the same user/process context as Goobers"
	case errors.As(cause, &start) && (start.Op == "fork/exec" || start.Op == "CreateProcess"):
		detail = "process could not start"
		hint = "check the Azure CLI installation, launcher and executable permissions in the same user/process context as Goobers"
	case errors.As(cause, &exit):
		detail = "process exited unsuccessfully"
		if code := exit.ExitCode(); code >= 0 {
			detail = fmt.Sprintf("process exited with code %d", code)
		}
	}
	return &azureCLICommandFailure{
		message: "azure CLI get-access-token: " + detail + "; " + hint,
		cause:   cause,
	}
}
