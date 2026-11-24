package anthropic

import "unicode/utf8"

// CountTokens provides an approximate token estimate for Anthropic Claude text.
//
// @purpose Computes token length heuristics (averaging 3.5 characters per token).
// @rationale Provides fast, zero-allocation token metrics when full external tokenizers are unnecessary.
// @preconditions text must be UTF-8 valid.
// - @param: text String input to tokenize.
// @returns Estimated token count (minimum 1 token if non-empty).
// @throws None.
// @invariants O(N) where N is rune count of string.
func CountTokens(text string) int {
	if len(text) == 0 {
		return 0
	}
	charCount := utf8.RuneCountInString(text)
	tokens := int(float64(charCount) / 3.5)
	if tokens < 1 {
		return 1
	}
	return tokens
}
