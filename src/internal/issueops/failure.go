package issueops

import "skein-api/internal/issues"

// Failure is the refusal of an operation: the code (the closed set of
// errcode, the same one MCP and the public API answer), a message for a
// human and the members the refusal carries.
type Failure struct {
	Code    string
	Message string
	Fields  map[string]any
}

// Error implements error.
func (f *Failure) Error() string { return f.Code + ": " + f.Message }

// Fail is a refusal with code, message and fields.
func Fail(code, message string, fields map[string]any) *Failure {
	return &Failure{Code: code, Message: message, Fields: fields}
}

// ValidationFailure is the refusal of a value an issues validator refused:
// validation_error with the validator's own code as the code member — or,
// for blocked_is_derived, that code itself.
func ValidationFailure(err error) *Failure {
	if ve, ok := issues.IsValidationError(err); ok {
		if ve.Code == "blocked_is_derived" {
			return Fail(ve.Code, ve.Message, nil)
		}
		return Fail(codeValidationError, ve.Message, map[string]any{"code": ve.Code})
	}
	return Fail(codeValidationError, err.Error(), nil)
}
