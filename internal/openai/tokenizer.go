package openai

import "unicode/utf8"

// CountTokens provides ultra-fast token estimation for OpenAI telemetry headers and usage.
func CountTokens(text string) int {
	if text == "" {
		return 0
	}
	// Heuristic standard: ~4 characters per token in English / code
	runeCount := utf8.RuneCountInString(text)
	tokens := runeCount / 4
	if tokens < 1 {
		tokens = 1
	}
	return tokens
}
