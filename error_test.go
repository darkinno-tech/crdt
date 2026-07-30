package crdt

import (
	"errors"
	"testing"
)

func TestStructuredErrorPreservesCauseAndCode(t *testing.T) {
	cause := errors.New("unsafe configuration")
	err := WrapError(ErrorCodeInvalidConfig, "durable.new_handler", cause)
	if !errors.Is(err, cause) {
		t.Fatalf("errors.Is() = false, want original cause")
	}
	if ErrorCodeOf(err) != ErrorCodeInvalidConfig {
		t.Fatalf("ErrorCodeOf() = %q, want %q", ErrorCodeOf(err), ErrorCodeInvalidConfig)
	}
	if !errors.Is(err, &Error{Code: ErrorCodeInvalidConfig}) {
		t.Fatalf("errors.Is() = false, want matching structured code")
	}
	var structured *Error
	if !errors.As(err, &structured) || structured.Operation != "durable.new_handler" {
		t.Fatalf("errors.As() = %#v, want operation", structured)
	}
}

func TestStructuredErrorNilAndUnknown(t *testing.T) {
	if got := WrapError(ErrorCodeInvalidInput, "ignored", nil); got != nil {
		t.Fatalf("WrapError(nil cause) = %v, want nil", got)
	}
	if got := ErrorCodeOf(errors.New("plain")); got != ErrorCodeUnknown {
		t.Fatalf("ErrorCodeOf(plain) = %q, want %q", got, ErrorCodeUnknown)
	}
}

func TestStructuredErrorFormatsAndHandlesNil(t *testing.T) {
	var nilError *Error
	if got := nilError.Error(); got != "<nil>" {
		t.Fatalf("nil Error() = %q", got)
	}
	if got := nilError.Unwrap(); got != nil {
		t.Fatalf("nil Unwrap() = %v", got)
	}
	if nilError.Is(&Error{Code: ErrorCodeInvalidConfig}) {
		t.Fatal("nil Error matched a code")
	}
	if (&Error{Code: ErrorCodeInvalidConfig}).Is((*Error)(nil)) {
		t.Fatal("Error matched a nil target")
	}

	cause := errors.New("cause")
	for _, test := range []struct {
		name  string
		error *Error
		want  string
	}{
		{"code only", &Error{Code: ErrorCodeConflict}, "conflict"},
		{"operation only", &Error{Code: ErrorCodeConflict, Operation: "relay.append"}, "conflict: relay.append"},
		{"cause only", &Error{Code: ErrorCodeConflict, Cause: cause}, "conflict: cause"},
		{"full", &Error{Code: ErrorCodeConflict, Operation: "relay.append", Cause: cause}, "conflict: relay.append: cause"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := test.error.Error(); got != test.want {
				t.Fatalf("Error() = %q, want %q", got, test.want)
			}
		})
	}
}
