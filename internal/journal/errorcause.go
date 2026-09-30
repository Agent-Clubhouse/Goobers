package journal

import (
	"strings"
)

const maxErrorCauses = 32

type stageErrorCoder interface {
	StageErrorCode() string
}

type errorCoder interface {
	ErrorCode() string
}

type codeProvider interface {
	Code() string
}

type errorClasser interface {
	ErrorClass() string
}

type classProvider interface {
	Class() string
}

type singleUnwrapper interface {
	Unwrap() error
}

type multiUnwrapper interface {
	Unwrap() []error
}

// ErrorDetailFor records err as an ErrorDetail with an ordered cause chain
// derived from errors.Unwrap. Legacy callers that only know flat text should
// keep constructing ErrorDetail directly, leaving Causes absent rather than
// fabricating structure.
func ErrorDetailFor(code string, err error) *ErrorDetail {
	if err == nil {
		return nil
	}
	return &ErrorDetail{Code: code, Message: err.Error(), Causes: ErrorCauses(err)}
}

// ErrorCauses returns the wrapped error chain as ordered causal layers. The
// boundaries come only from errors.Unwrap/Unwrap() []error; punctuation is not
// split to invent structure.
func ErrorCauses(err error) []ErrorCause {
	if err == nil {
		return nil
	}
	var causes []ErrorCause
	appendErrorCauses(&causes, err)
	return causes
}

func appendErrorCauses(causes *[]ErrorCause, err error) {
	if err == nil || len(*causes) >= maxErrorCauses {
		return
	}
	switch unwrapped := any(err).(type) {
	case multiUnwrapper:
		*causes = append(*causes, errorCauseFor(err, err.Error()))
		for _, child := range unwrapped.Unwrap() {
			appendErrorCauses(causes, child)
			if len(*causes) >= maxErrorCauses {
				return
			}
		}
	case singleUnwrapper:
		child := unwrapped.Unwrap()
		cause := errorCauseFor(err, wrappedLayerMessage(err, child))
		*causes = append(*causes, cause)
		if child != nil && cause.Message == child.Error() && (cause.Code != "" || cause.Class != "") {
			return
		}
		appendErrorCauses(causes, child)
	default:
		*causes = append(*causes, errorCauseFor(err, err.Error()))
	}
}

func errorCauseFor(err error, message string) ErrorCause {
	return ErrorCause{
		Code:    errorCauseCode(err),
		Class:   errorCauseClass(err),
		Message: strings.TrimSpace(message),
	}
}

func wrappedLayerMessage(err, child error) string {
	if err == nil {
		return ""
	}
	message := err.Error()
	if child == nil {
		return message
	}
	childMessage := child.Error()
	if childMessage == "" || !strings.HasSuffix(message, childMessage) {
		return message
	}
	layer := strings.TrimSpace(strings.TrimSuffix(message, childMessage))
	if strings.HasSuffix(layer, ":") {
		layer = strings.TrimSpace(strings.TrimSuffix(layer, ":"))
	}
	if layer == "" {
		return message
	}
	return layer
}

func errorCauseCode(err error) string {
	if stage, ok := err.(stageErrorCoder); ok {
		if code := strings.TrimSpace(stage.StageErrorCode()); code != "" {
			return code
		}
	}
	if errorCode, ok := err.(errorCoder); ok {
		if code := strings.TrimSpace(errorCode.ErrorCode()); code != "" {
			return code
		}
	}
	if provider, ok := err.(codeProvider); ok {
		return strings.TrimSpace(provider.Code())
	}
	return ""
}

func errorCauseClass(err error) string {
	if errorClass, ok := err.(errorClasser); ok {
		if class := strings.TrimSpace(errorClass.ErrorClass()); class != "" {
			return class
		}
	}
	if provider, ok := err.(classProvider); ok {
		return strings.TrimSpace(provider.Class())
	}
	return ""
}
