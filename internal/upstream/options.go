// Per-request upstream options: search/thinking toggles, reasoning
// effort level, and pre-uploaded vision files. Written fresh for this
// repo; semantics mirror the GLM-ZAI-2API feature handling.
package upstream

import (
	"log"
	"strings"
)

// ChatOpts carries optional per-request knobs from the API layer to the
// Z.AI request builder. Nil pointers mean "leave the default alone".
type ChatOpts struct {
	WebSearch *bool
	Thinking  *bool
	// ReasoningEffort is one of low/high/max; honored only on models
	// whose capabilities declare it (otherwise ignored with a log).
	ReasoningEffort string
	// Files holds uploaded vision file entries for the top-level
	// "files" array. Nil for text-only requests.
	Files []map[string]interface{}
}

// supportsReasoningEffort reports whether effort levels apply to a model.
func supportsReasoningEffort(modelID string) bool {
	switch strings.ToLower(strings.TrimSpace(modelID)) {
	case "glm-5.2", "glm-5.3", "glm-5":
		return true
	}
	return false
}

// validEffort validates effort levels.
func validEffort(effort string) bool {
	switch strings.ToLower(strings.TrimSpace(effort)) {
	case "low", "high", "max":
		return true
	}
	return false
}

// applyOpts folds per-request options into the feature map.
func applyOpts(features map[string]interface{}, model string, opts ChatOpts) {
	if opts.WebSearch != nil {
		if *opts.WebSearch {
			features["auto_web_search"] = true
			features["web_search"] = true
		} else {
			delete(features, "auto_web_search")
			delete(features, "web_search")
		}
	}
	if opts.Thinking != nil {
		features["enable_thinking"] = *opts.Thinking
	}
	effort := strings.ToLower(strings.TrimSpace(opts.ReasoningEffort))
	delete(features, "reasoning_effort")
	if effort == "" {
		return
	}
	if !supportsReasoningEffort(model) {
		log.Printf("[reasoning_effort] model=%s ignores effort (unsupported)", model)
		return
	}
	if !validEffort(effort) {
		log.Printf("[reasoning_effort] invalid value %q (accepted: low, high, max); ignored", opts.ReasoningEffort)
		return
	}
	features["reasoning_effort"] = effort
	features["enable_thinking"] = true
	log.Printf("[reasoning_effort] model=%s effort=%s", model, effort)
}

// FirstTrue returns the first non-nil bool pointer, or nil.
func FirstTrue(flags ...*bool) *bool {
	for _, f := range flags {
		if f != nil {
			return f
		}
	}
	return nil
}
