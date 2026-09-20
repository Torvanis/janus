package detect

import (
	"encoding/json"
	"strconv"
)

// ShapeLimits bounds the structure of an OpenAI-style chat completion
// request. A zero value for any limit means "unlimited".
type ShapeLimits struct {
	MaxMessages   int
	MaxBodyBytes  int
	MaxImageParts int
	MaxTools      int
	// DenySystemFromServiceTokens rejects requests that carry a system (or
	// developer) message when authenticated with a service token.
	DenySystemFromServiceTokens bool
}

// ShapeInput is the request under inspection.
type ShapeInput struct {
	Body           []byte
	IsServiceToken bool
}

// shapeBody is the minimal projection of a chat request needed by CheckShape.
type shapeBody struct {
	Messages []shapeMessage    `json:"messages"`
	Tools    []json.RawMessage `json:"tools"`
}

type shapeMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type shapePart struct {
	Type string `json:"type"`
}

// CheckShape returns one Match per violated limit. Offsets are always zero
// because shape violations concern the request as a whole rather than a
// span. A body that is not valid JSON can only trip max_body_bytes.
func CheckShape(in ShapeInput, l ShapeLimits) []Match {
	var out []Match
	add := func(rule string, sev Severity, text string) {
		out = append(out, Match{Kind: KindShape, RuleID: rule, Severity: sev, Text: text})
	}
	if l.MaxBodyBytes > 0 && len(in.Body) > l.MaxBodyBytes {
		add("max_body_bytes", SeverityMedium,
			strconv.Itoa(len(in.Body))+" body bytes exceeds the limit of "+strconv.Itoa(l.MaxBodyBytes))
	}
	var body shapeBody
	if err := json.Unmarshal(in.Body, &body); err != nil {
		return out
	}
	if l.MaxMessages > 0 && len(body.Messages) > l.MaxMessages {
		add("max_messages", SeverityMedium,
			strconv.Itoa(len(body.Messages))+" messages exceeds the limit of "+strconv.Itoa(l.MaxMessages))
	}
	if l.MaxTools > 0 && len(body.Tools) > l.MaxTools {
		add("max_tools", SeverityMedium,
			strconv.Itoa(len(body.Tools))+" tools exceeds the limit of "+strconv.Itoa(l.MaxTools))
	}
	images := 0
	hasSystem := false
	for _, m := range body.Messages {
		if m.Role == "system" || m.Role == "developer" {
			hasSystem = true
		}
		if len(m.Content) == 0 || m.Content[0] != '[' {
			continue
		}
		var parts []shapePart
		if err := json.Unmarshal(m.Content, &parts); err != nil {
			continue
		}
		for _, p := range parts {
			if p.Type == "image_url" || p.Type == "input_image" {
				images++
			}
		}
	}
	if l.MaxImageParts > 0 && images > l.MaxImageParts {
		add("max_image_parts", SeverityMedium,
			strconv.Itoa(images)+" image parts exceeds the limit of "+strconv.Itoa(l.MaxImageParts))
	}
	if l.DenySystemFromServiceTokens && in.IsServiceToken && hasSystem {
		add("system_from_service_token", SeverityHigh,
			"system message is not permitted for service token requests")
	}
	return out
}
