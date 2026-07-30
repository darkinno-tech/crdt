package crdt

import "errors"

// ErrorCode classifies a failure without requiring callers to parse an error
// message. Codes describe a stable operational category, not a wire protocol
// result or application authorization policy.
type ErrorCode string

const (
	// ErrorCodeUnknown is returned when an error has no CRDT structured wrapper.
	ErrorCodeUnknown ErrorCode = "unknown"
	// ErrorCodeInvalidConfig identifies missing, malformed, or unsafe local
	// configuration before an operation begins.
	ErrorCodeInvalidConfig ErrorCode = "invalid_config"
	// ErrorCodeInvalidInput identifies rejected untrusted or malformed input.
	ErrorCodeInvalidInput ErrorCode = "invalid_input"
	// ErrorCodeUnauthorized identifies an authentication or authorization denial.
	ErrorCodeUnauthorized ErrorCode = "unauthorized"
	// ErrorCodeConflict identifies an incompatible retry or concurrent binding.
	ErrorCodeConflict ErrorCode = "conflict"
	// ErrorCodeResourceLimit identifies a configured capacity or size bound.
	ErrorCodeResourceLimit ErrorCode = "resource_limit"
	// ErrorCodeUnavailable identifies a closed or temporarily unavailable dependency.
	ErrorCodeUnavailable ErrorCode = "unavailable"
)

// Error adds an operation and a stable code to a cause. Operation must be a
// constant diagnostic name such as "durable.new_handler"; do not put peer IDs,
// group IDs, endpoints, credentials, payloads, or other untrusted data in it.
//
// Error unwraps to Cause, so existing errors.Is and errors.As checks continue
// to work when a package adopts structured errors at a public boundary.
type Error struct {
	Code      ErrorCode
	Operation string
	Cause     error
}

// Error implements error.
func (e *Error) Error() string {
	if e == nil {
		return "<nil>"
	}
	if e.Cause == nil {
		if e.Operation == "" {
			return string(e.Code)
		}
		return string(e.Code) + ": " + e.Operation
	}
	if e.Operation == "" {
		return string(e.Code) + ": " + e.Cause.Error()
	}
	return string(e.Code) + ": " + e.Operation + ": " + e.Cause.Error()
}

// Unwrap returns the original cause for errors.Is and errors.As compatibility.
func (e *Error) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

// Is matches another structured Error by its non-empty code. It deliberately
// does not compare operation names, which are diagnostic context rather than a
// compatibility contract.
func (e *Error) Is(target error) bool {
	other, ok := target.(*Error)
	return ok && other != nil && e != nil && e.Code != "" && e.Code == other.Code
}

// WrapError adds a stable code and operation to cause. It returns nil when
// cause is nil so callers can use it in ordinary error-returning paths.
func WrapError(code ErrorCode, operation string, cause error) error {
	if cause == nil {
		return nil
	}
	return &Error{Code: code, Operation: operation, Cause: cause}
}

// ErrorCodeOf returns the outermost structured error code in err's tree, or
// ErrorCodeUnknown when no structured wrapper is present.
func ErrorCodeOf(err error) ErrorCode {
	var structured *Error
	if errors.As(err, &structured) && structured != nil && structured.Code != "" {
		return structured.Code
	}
	return ErrorCodeUnknown
}
