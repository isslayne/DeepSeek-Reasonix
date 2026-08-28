package agent

import (
	"errors"
	"fmt"
	"strings"
)

// ErrCompletionRejected identifies a provider response that reached a terminal
// transport state but is not safe to commit as a complete assistant turn.
var ErrCompletionRejected = errors.New("provider completion rejected")

// CompletionRejectedError describes why a provider completion was kept out of
// canonical session history and tool execution.
type CompletionRejectedError struct {
	FinishReason string
}

func (e *CompletionRejectedError) Error() string {
	return fmt.Sprintf("%v: non-committable finish reason %q", ErrCompletionRejected, e.FinishReason)
}

func (e *CompletionRejectedError) Unwrap() error { return ErrCompletionRejected }

// admitCompletion is the single semantic terminal gate shared by completion
// consumers. This first-stage gate rejects explicit non-committable reasons;
// empty and provider-specific reasons remain compatible until adapters expose
// a fully normalized typed terminal contract.
func admitCompletion(providerFinishReason string) error {
	reason := strings.ToLower(strings.TrimSpace(providerFinishReason))
	switch reason {
	case "length", "max_tokens", "max_output_tokens", "incomplete", "content_filter", "repetition_truncation":
		return &CompletionRejectedError{FinishReason: reason}
	default:
		return nil
	}
}
