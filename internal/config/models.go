package config

import "strings"

// ThoughtLevelLow, ThoughtLevelMedium, ThoughtLevelHigh and ThoughtLevelXHigh are the ONLY wire values
// accepted by agy acp for session/set_config_option{configId:"thought_level"}.
//
// @purpose Single source of truth for the thought-level enum.
// @rationale Verified against agy 1.5.16: "off", "minimal", "max" and "extra" are all rejected with
// -32602. "extra" is merely the DISPLAY LABEL of the wire value "xhigh" (advertised as
// {"value":"xhigh","name":"Extra"}), so returning the label as a wire value silently downgrades every
// high-effort request to the session default.
const (
	ThoughtLevelLow    = "low"
	ThoughtLevelMedium = "medium"
	ThoughtLevelHigh   = "high"
	ThoughtLevelXHigh  = "xhigh"
)

// ThoughtLevels lists the accepted wire values in ascending effort order.
var ThoughtLevels = []string{ThoughtLevelLow, ThoughtLevelMedium, ThoughtLevelHigh, ThoughtLevelXHigh}

// MapReasoningEffort converts a client reasoning-effort token into an agy acp thought_level wire value.
//
// @purpose Normalises OpenAI `reasoning_effort` and Anthropic thinking budgets into a value agy accepts.
// @rationale The gateway previously emitted "off" and "extra", both of which agy rejects with -32602, and
// the error was discarded, so callers silently received the session default instead of the level they asked
// for. Mapping onto the four accepted wire values makes every request land where the caller intended.
//
// @preconditions effort and defaultEffort are free-form; case and surrounding space are ignored.
// @param effort Client-supplied effort token ("none", "minimal", "low", "medium", "high", "xhigh", "extra").
// @param defaultEffort Fallback used when effort is empty.
// @returns One of ThoughtLevels; never "off" and never "extra".
// @throws None.
// @invariants Pure function, O(1), always returns an accepted wire value.
func MapReasoningEffort(effort string, defaultEffort string) string {
	target := strings.ToLower(strings.TrimSpace(effort))
	if target == "" {
		target = strings.ToLower(strings.TrimSpace(defaultEffort))
	}

	switch target {
	case "none", "off", "minimal", ThoughtLevelLow:
		// agy offers no "off"/"none" level; the lowest accepted effort is "low".
		return ThoughtLevelLow
	case ThoughtLevelMedium:
		return ThoughtLevelMedium
	case ThoughtLevelHigh:
		return ThoughtLevelHigh
	case ThoughtLevelXHigh, "extra":
		// "extra" is the display label of the "xhigh" wire value.
		return ThoughtLevelXHigh
	case "max":
		return ThoughtLevelXHigh
	default:
		return ThoughtLevelMedium
	}
}

// MapThinkingBudget converts an Anthropic thinking budget into an agy thought_level wire value.
//
// @purpose Bridges Anthropic `thinking.budget_tokens` onto the four accepted ACP effort levels.
// @rationale Anthropic expresses effort as a token budget with no ACP equivalent, so a deterministic
// bucketing is required instead of dropping the field.
//
// @param budgetTokens Requested reasoning budget; <= 0 means unset.
// @returns One of ThoughtLevels (medium when the budget is unset).
// @invariants Pure, O(1), monotonic non-decreasing in budgetTokens.
func MapThinkingBudget(budgetTokens int) string {
	switch {
	case budgetTokens <= 0:
		return ThoughtLevelMedium
	case budgetTokens <= 1024:
		return ThoughtLevelLow
	case budgetTokens <= 4096:
		return ThoughtLevelMedium
	case budgetTokens <= 16384:
		return ThoughtLevelHigh
	default:
		return ThoughtLevelXHigh
	}
}

// IsThoughtLevel reports whether value is an accepted agy thought_level wire value.
//
// @purpose Validates a value before it is sent upstream.
// @param value Candidate wire value.
// @returns True for low/medium/high/xhigh (case-insensitive).
// @invariants Pure, O(4).
func IsThoughtLevel(value string) bool {
	v := strings.ToLower(strings.TrimSpace(value))
	for _, l := range ThoughtLevels {
		if v == l {
			return true
		}
	}
	return false
}

// visionModes enumerates image/embedded-content handling.
//
// @purpose Validation set for execution.vision_mode.
// @rationale agy advertises image support but fails every such turn with a 400 surfaced as assistant text.
var visionModes = []string{"reject", "flatten"}

// allocationStrategies enumerates how the pool assigns accounts to workers.
//
// @purpose Validation set for accounts.allocation_strategy.
// @rationale The value was previously unused, so any typo silently behaved like the hardcoded
// round-robin. Round robin spreads load; least_used follows live quota; sticky avoids churn on a
// worker that already holds a usable account.
var allocationStrategies = []string{
	AllocationStrategyRoundRobin, AllocationStrategyLeastUsed, AllocationStrategySticky,
}

// AllocationStrategy* are the supported pool allocation policies.
//
// @purpose Gives callers a compile-time-checked name instead of repeating string literals, which is
// how the value previously drifted from its documented behaviour without anyone noticing.
// @rationale `sticky` in particular is load-bearing: the pool persists worker affinity to disk, and
// the persistence layer must compare against the same constant the config validator accepts.
// @preconditions None.
// @param None.
// @returns None.
// @throws None.
// @invariants Immutable constants; safe for concurrent use.
const (
	// AllocationStrategyRoundRobin spreads distinct accounts across workers by index.
	AllocationStrategyRoundRobin = "round_robin"
	// AllocationStrategyLeastUsed is quota-aware greedy over live headroom.
	AllocationStrategyLeastUsed = "least_used"
	// AllocationStrategySticky keeps each worker on its account across restarts.
	AllocationStrategySticky = "sticky"
)

// allocationStrategyLegacyAliases maps values that existed before the knob was wired up.
//
// @purpose Keeps existing config files and containers working after the setting became effective.
// @rationale "dedicated_per_worker" was the shipped default while the field did nothing; rejecting it
// now would break a running deployment on restart for no behavioural reason.
var allocationStrategyLegacyAliases = map[string]string{
	"dedicated_per_worker": "round_robin",
}

// NormalizeAllocationStrategy maps a configured value onto a supported strategy.
//
// @purpose Single point where a legacy or empty value becomes a concrete strategy.
// @rationale Validation and runtime selection must agree; normalising once avoids two code paths that
// can disagree about what an unset value means.
// @preconditions None.
// @param v Raw configured value.
// @returns A supported strategy name; empty input yields "round_robin".
// @throws None; unknown values are returned unchanged so Validate can reject them.
// @invariants O(1); no allocation beyond one map lookup.
func NormalizeAllocationStrategy(v string) string {
	v = strings.ToLower(strings.TrimSpace(v))
	if v == "" {
		return AllocationStrategyRoundRobin
	}
	if canonical, ok := allocationStrategyLegacyAliases[v]; ok {
		return canonical
	}
	return v
}

func validAllocationStrategy(v string) bool {
	return contains(allocationStrategies, NormalizeAllocationStrategy(v))
}

func validVisionMode(v string) bool { return contains(visionModes, v) }

// contains reports membership in a small string set.
//
// @purpose Shared validation helper for closed enums.
// @param set Candidate values.
// @param v Value to look up.
// @returns True when v is a member.
// @invariants O(n) over a compile-time constant slice.
func contains(set []string, v string) bool {
	for _, s := range set {
		if s == v {
			return true
		}
	}
	return false
}
