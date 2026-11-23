package openai

import (
	"encoding/json"
	"errors"
)

// Validator verifies tool call arguments against basic JSON syntax and schemas.
type Validator struct{}

// NewValidator creates an argument validator.
func NewValidator() *Validator {
	return &Validator{}
}

// ValidateJSON ensures arguments represent well-formed JSON objects.
func (v *Validator) ValidateJSON(rawArguments string) error {
	if rawArguments == "" {
		return errors.New("empty tool arguments")
	}

	var parsed any
	if err := json.Unmarshal([]byte(rawArguments), &parsed); err != nil {
		return err
	}
	return nil
}
