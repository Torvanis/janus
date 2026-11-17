package subscription

import (
	"strings"

	"github.com/torvanis/janus/internal/adapter"
	"github.com/torvanis/janus/internal/capability"
)

// Built-in capability defaults for personal-plan models.
//
// Organization models follow a strict rule: nothing is inferred from a
// model's name; a capability is known only when the provider's list says
// it, a probe measured it or an administrator set it. Personal plans are
// the explicit exception (owner ruling): there are only a handful of plan
// vendors, each documents what its plan models take, and a person should
// not have to measure a model before Chat sends it a picture. So each
// vendor gets a short table, per model family, of what its documentation
// says the model accepts THROUGH THE ROUTE JANUS USES FOR THAT VENDOR.
//
// These are the weakest layer of a plan model's record ("Assumed for
// <vendor> plans"): whatever the vendor's own model list reports, what the
// owner's Measure finds, and what the owner sets all replace them. A key
// a vendor does not document stays unknown, which Chat treats as "do not
// send it directly" (an image is described, sound transcribed).
//
// Every entry carries its source in Note, shown to the person beside the
// value. Re-check the sources when a vendor ships a new model family.

// planRule is one row of a vendor's table: models whose native name starts
// with one of prefixes (all models when empty) get facts.
type planRule struct {
	prefixes []string
	facts    map[string]any
	note     string
}

// planDefaults is the table, per provider ID. Rules are tried in order and
// every matching rule applies (an earlier rule wins a key both set), so a
// vendor-wide route fact can sit under per-family rows.
var planDefaults = map[string][]planRule{
	// OpenAI ChatGPT plan: Janus calls the Codex Responses backend through
	// its codex adapter, which turns image_url parts into input_image and
	// refuses input_audio / video_url before sending (adapter/codex.go).
	// Codex's own model catalog lists input_modalities ["text","image"]
	// for its models and defaults the field to both (openai/codex
	// codex-rs/models-manager/models.json, checked 2026-11-15); when the
	// plan's catalog sends the field, that report replaces these rows.
	"openai": {
		{
			prefixes: []string{"gpt-5", "gpt-6", "codex-"},
			facts:    map[string]any{adapter.CapInImage: true, adapter.CapTools: true},
			note:     "Codex's model catalog lists text and image input for GPT-5 and later; Janus's ChatGPT route sends images as input_image and carries function tools.",
		},
		{
			facts: map[string]any{adapter.CapInAudio: false, adapter.CapInVideo: false},
			note:  "Janus's ChatGPT-plan route (the Codex backend) cannot carry sound or video; Chat transcribes or describes them instead.",
		},
	},
	// xAI SuperGrok / X Premium+: api.x.ai chat completions through the
	// OpenAI-compatible adapter. docs.x.ai model pages list "Modalities:
	// text, image → text" and "Function calling: Yes" for grok-4.3, 4.5,
	// 4.6, 4.7, 4.20 (reasoning, non-reasoning, multi-agent) and
	// grok-build-0.1 / grok-code (checked 2026-11-15). Sound and video go
	// to xAI's separate Voice and Imagine APIs, not chat.
	"xai": {
		{
			prefixes: []string{"grok-4", "grok-build", "grok-code"},
			facts: map[string]any{
				adapter.CapInImage: true, adapter.CapInAudio: false, adapter.CapInVideo: false, adapter.CapTools: true,
			},
			note: "xAI's model pages (docs.x.ai) list text and image input, no sound or video, and function calling for Grok 4 and Grok Build models.",
		},
	},
	// GitHub Copilot: the plan's own model list reports vision, tool calls
	// and structured output per model (capabilities.supports), so nothing
	// is assumed; GitHub documents no sound or video input for Copilot's
	// chat endpoint, and that stays unknown until measured.
	"copilot": nil,
	// Mistral Vibe: Mistral's model list reports vision and
	// function_calling per model (capabilities), so nothing is assumed.
	"mistral": nil,
}

// PlanDefaults returns the built-in capability defaults for one plan model:
// what providerID's documentation says model takes through Janus's route
// for that vendor. Empty when nothing is documented.
func PlanDefaults(providerID, model string) capability.Facts {
	out := capability.Facts{}
	name := strings.ToLower(strings.TrimSpace(model))
	for _, rule := range planDefaults[providerID] {
		if !planRuleMatches(rule, name) {
			continue
		}
		for k, v := range rule.facts {
			if _, set := out[k]; set {
				continue
			}
			if n, err := capability.Normalize(k, v); err == nil {
				out[k] = capability.Fact{Value: n, Note: rule.note}
			}
		}
	}
	return out
}

func planRuleMatches(rule planRule, name string) bool {
	if len(rule.prefixes) == 0 {
		return true
	}
	for _, p := range rule.prefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// PlanAutoMeasure lists the providers whose models Janus measures by itself
// when their owner turns one on. It is empty: every plan vendor counts
// requests against the person's plan (Copilot Free has a small monthly
// chat allowance a ~30-request measurement would visibly dent), so plan
// models start from the built-in defaults and the vendor's own list, and
// the owner presses Measure when they want more. Add a provider ID here to
// measure its models on selection.
var PlanAutoMeasure = map[string]bool{}
