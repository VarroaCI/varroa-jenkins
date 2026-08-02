package apikey

import (
	"errors"
	"fmt"
)

// RotateError is returned when rotation partially completes (new key created
// but old key could not be deleted). The caller receives the new token and
// must separately revoke the old prefix.
type RotateError struct {
	NewToken string
	Err      error
}

func (e *RotateError) Error() string {
	return fmt.Sprintf("rotate partial: %v", e.Err)
}

func (e *RotateError) Unwrap() error {
	return e.Err
}

// ErrUnavailable is returned when verification cannot proceed due to an
// infrastructure failure (e.g., Kubernetes API error). It is distinct from
// credential errors so callers can distinguish "try again later" from
// "definitively rejected".
var ErrUnavailable = errors.New("verification temporarily unavailable")
