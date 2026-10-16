package adapter

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
)

// Reasoning effort on Anthropic.
//
// OpenAI-shaped clients (Open WebUI, LangChain, the OpenAI SDKs) ask for
// thinking with reasoning_effort ("low" … "xhigh") or reasoning.effort. The
// Messages API has two incompatible ways to ask, depending on the model:
//
//   - Claude 4.6 and later: thinking {type: "adaptive"} with
//     output_config.effort (low | medium | high | xhigh | max). From Claude
//     5 on this is the only form: {type: "enabled"} and {type: "disabled"}
//     are refused ("thinking.type.enabled is not supported for this model").
//   - Earlier Claude: thinking {type: "enabled", budget_tokens: N}, with
//     max_tokens above the budget; adaptive is refused.
//
// Before this translation reasoning_effort was dropped, so no level ever
// reached Claude. A caller that sends Anthropic's own `thinking` object keeps
// full control: it passes through and no effort is translated.
//
// Thinking also changes what sampling the API accepts: temperature must be
// 1 and top_k is refused, so both are dropped (they are sampling hints; the
// answer's meaning does not depend on them). Values Claude refuses
// otherwise (Claude 5 deprecates temperature and top_p) are learned from the
// refusal by the proxy and dropped from then on.

// claudeVersion reads the generation from a Claude model id:
// "claude-opus-5-5" → 5.5, "claude-sonnet-4-5-20250929" → 4.5,
// "claude-opus-5" → 5.0, "claude-3-7-sonnet-20250219" → 3.7. ok is false
// for ids that do not look like Claude.
var (
	reClaudeFamilyFirst = regexp.MustCompile(`(?i)claude-[a-z]+-(\d+)(?:[-.](\d{1,2}))?(?:$|[-@:])`)
	reClaudeNumberFirst = regexp.MustCompile(`(?i)claude-(\d+)(?:[-.](\d{1,2}))?-[a-z]`)
)

func claudeVersion(model string) (major, minor int, ok bool) {
	m := reClaudeFamilyFirst.FindStringSubmatch(model)
	if m == nil {
		m = reClaudeNumberFirst.FindStringSubmatch(model)
	}
	if m == nil {
		return 0, 0, false
	}
	major, _ = strconv.Atoi(m[1])
	if m[2] != "" {
		minor, _ = strconv.Atoi(m[2])
	}
	return major, minor, true
}

// claudeAdaptive reports whether a model takes adaptive thinking with an
// effort (4.6 and later). Unknown ids are treated as current models.
func claudeAdaptive(model string) bool {
	major, minor, ok := claudeVersion(model)
	if !ok {
		return true
	}
	return major > 4 || (major == 4 && minor >= 6)
}

// claudeMaxOutput is the output-token limit of the budget-form Claude
// models: 32,000 for Opus 4 and 4.1, 64,000 for the others.
func claudeMaxOutput(model string) int {
	major, minor, ok := claudeVersion(model)
	if ok && major == 4 && minor <= 1 && strings.Contains(strings.ToLower(model), "opus") {
		return 32000
	}
	return 64000
}

// AnthropicThinkingBudgets maps a reasoning effort to an extended-thinking
// token budget for Claude models that take budget_tokens.
var AnthropicThinkingBudgets = map[string]int{"minimal": 1024, "low": 2048, "medium": 8192, "high": 16384, "xhigh": 32000, "max": 63999}

// anthropicAdaptiveMaxTokens is the output cap used with adaptive thinking
// when the caller named none: 4096 leaves almost no room once Claude thinks.
const anthropicAdaptiveMaxTokens = 32000

// chatEffort reads the requested effort from a chat body: reasoning_effort
// or reasoning.effort, lower-cased. "" when none was asked for.
func chatEffort(reasoningEffort string, reasoning json.RawMessage) string {
	effort := reasoningEffort
	if effort == "" && len(reasoning) > 0 {
		var r struct {
			Effort string `json:"effort"`
		}
		_ = json.Unmarshal(reasoning, &r)
		effort = r.Effort
	}
	return strings.ToLower(strings.TrimSpace(effort))
}

// applyAnthropicEffort writes the thinking request for effort onto a
// Messages body (out), for the given model. explicitCap reports whether the
// caller named an output cap.
func applyAnthropicEffort(out map[string]any, model, effort string, explicitCap bool) {
	if effort == "" {
		return
	}
	maxTokens, _ := out["max_tokens"].(int)
	if claudeAdaptive(model) {
		// Adaptive models cannot switch thinking off; "none" and
		// "minimal" ask for the least effort they take.
		level := effort
		switch effort {
		case "none", "minimal":
			level = "low"
		case "ultra":
			level = "max"
		}
		if effort != "none" {
			// Claude 5 omits the thinking text unless asked for a
			// summary; chat clients show it as reasoning_content.
			out["thinking"] = map[string]any{"type": "adaptive", "display": "summarized"}
		}
		out["output_config"] = map[string]any{"effort": level}
		if !explicitCap && maxTokens < anthropicAdaptiveMaxTokens && effort != "none" {
			out["max_tokens"] = anthropicAdaptiveMaxTokens
		}
	} else {
		if effort == "none" {
			return // thinking stays off
		}
		budget := AnthropicThinkingBudgets[effort]
		if budget == 0 {
			budget = AnthropicThinkingBudgets["medium"]
		}
		// Budget plus answer must fit the model's output limit, or the
		// request is refused ("max_tokens: 68095 > 64000").
		limit := claudeMaxOutput(model)
		answer := max(min(maxTokens, limit/2), 1024)
		if maxTokens > budget {
			answer = maxTokens - budget
		}
		budget = min(budget, limit-answer)
		out["thinking"] = map[string]any{"type": "enabled", "budget_tokens": budget}
		out["max_tokens"] = min(max(maxTokens, budget+answer), limit)
	}
	if _, on := out["thinking"]; on {
		// Extended thinking requires the default temperature and refuses
		// top_k; top_p is only accepted between 0.95 and 1.
		delete(out, "temperature")
		delete(out, "top_k")
		if p, ok := out["top_p"].(float64); ok && p < 0.95 {
			delete(out, "top_p")
		}
	}
}
