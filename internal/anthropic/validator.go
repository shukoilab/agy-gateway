package anthropic

import (
	"errors"
	"fmt"
)

// ValidateMessageRequest enforces RFC compliance and mandatory parameters.
//
// @purpose Ensures that incoming MessageRequest structures adhere strictly to Anthropic invariants.
// @rationale Fails fast before acquiring worker pool leases or spawning subprocesses.
// @preconditions req must be non-nil.
// - @param: req Pointer to MessageRequest to validate.
// @returns Nil on valid request, or typed descriptive error.
// @throws None.
// @invariants Validates model presence, positive max_tokens, and non-empty messages array.
func ValidateMessageRequest(req *MessageRequest) error {
	if req == nil {
		return errors.New("request payload is nil")
	}

	if req.Model == "" {
		return errors.New("missing required field: model")
	}

	if req.MaxTokens <= 0 {
		return errors.New("missing or invalid required field: max_tokens must be greater than 0")
	}

	if len(req.Messages) == 0 {
		return errors.New("missing required field: messages array must not be empty")
	}

	for i, msg := range req.Messages {
		if msg.Role != "user" && msg.Role != "assistant" {
			return fmt.Errorf("invalid message role at index %d: %q (must be 'user' or 'assistant')", i, msg.Role)
		}
	}

	return nil
}
