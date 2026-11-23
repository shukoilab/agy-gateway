package config

import "strings"

// MapReasoningEffort converts OpenAI reasoning_effort string values
// into Antigravity ACP thought_level parameters.
//
// OpenAI values: "none", "minimal", "low", "medium", "high", "xhigh"
// ACP values: "off", "low", "medium", "high", "extra"
func MapReasoningEffort(effort string, defaultEffort string) string {
	target := strings.ToLower(strings.TrimSpace(effort))
	if target == "" {
		target = strings.ToLower(strings.TrimSpace(defaultEffort))
	}

	switch target {
	case "none", "off":
		return "off"
	case "minimal", "low":
		return "low"
	case "medium":
		return "medium"
	case "high":
		return "high"
	case "xhigh", "extra":
		return "extra"
	default:
		return "medium"
	}
}
