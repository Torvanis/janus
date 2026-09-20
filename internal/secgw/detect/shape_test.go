package detect

import (
	"strings"
	"testing"
)

func ruleIDs(ms []Match) []string {
	var out []string
	for _, m := range ms {
		if m.Kind != KindShape || m.Offset != 0 || m.Length != 0 {
			panic("shape match must have KindShape and zero span")
		}
		out = append(out, m.RuleID)
	}
	return out
}

func hasRule(ms []Match, id string) *Match {
	for i := range ms {
		if ms[i].RuleID == id {
			return &ms[i]
		}
	}
	return nil
}

const shapeBodySample = `{
  "model": "gpt-x",
  "messages": [
    {"role": "system", "content": "be nice"},
    {"role": "user", "content": [
      {"type": "text", "text": "look"},
      {"type": "image_url", "image_url": {"url": "data:..."}},
      {"type": "image_url", "image_url": {"url": "data:..."}}
    ]},
    {"role": "user", "content": "plain"}
  ],
  "tools": [{"type":"function"},{"type":"function"},{"type":"function"}]
}`

func TestShapeMaxMessages(t *testing.T) {
	ms := CheckShape(ShapeInput{Body: []byte(shapeBodySample)}, ShapeLimits{MaxMessages: 2})
	m := hasRule(ms, "max_messages")
	if m == nil || len(ms) != 1 {
		t.Fatalf("got %v", ruleIDs(ms))
	}
	if m.Text != "3 messages exceeds the limit of 2" {
		t.Fatalf("text %q", m.Text)
	}
	if ms := CheckShape(ShapeInput{Body: []byte(shapeBodySample)}, ShapeLimits{MaxMessages: 3}); len(ms) != 0 {
		t.Fatalf("at-limit should pass: %v", ruleIDs(ms))
	}
}

func TestShapeMaxBodyBytes(t *testing.T) {
	ms := CheckShape(ShapeInput{Body: []byte(shapeBodySample)}, ShapeLimits{MaxBodyBytes: 10})
	m := hasRule(ms, "max_body_bytes")
	if m == nil || len(ms) != 1 || !strings.Contains(m.Text, "exceeds the limit of 10") {
		t.Fatalf("got %+v", ms)
	}
}

func TestShapeMaxImageParts(t *testing.T) {
	ms := CheckShape(ShapeInput{Body: []byte(shapeBodySample)}, ShapeLimits{MaxImageParts: 1})
	m := hasRule(ms, "max_image_parts")
	if m == nil || len(ms) != 1 || m.Text != "2 image parts exceeds the limit of 1" {
		t.Fatalf("got %+v", ms)
	}
	if ms := CheckShape(ShapeInput{Body: []byte(shapeBodySample)}, ShapeLimits{MaxImageParts: 2}); len(ms) != 0 {
		t.Fatalf("at-limit should pass: %v", ruleIDs(ms))
	}
}

func TestShapeMaxTools(t *testing.T) {
	ms := CheckShape(ShapeInput{Body: []byte(shapeBodySample)}, ShapeLimits{MaxTools: 2})
	m := hasRule(ms, "max_tools")
	if m == nil || len(ms) != 1 || m.Text != "3 tools exceeds the limit of 2" {
		t.Fatalf("got %+v", ms)
	}
}

func TestShapeSystemFromServiceToken(t *testing.T) {
	l := ShapeLimits{DenySystemFromServiceTokens: true}
	ms := CheckShape(ShapeInput{Body: []byte(shapeBodySample), IsServiceToken: true}, l)
	if m := hasRule(ms, "system_from_service_token"); m == nil || len(ms) != 1 || m.Severity != SeverityHigh {
		t.Fatalf("got %+v", ms)
	}
	// Not a service token → allowed.
	if ms := CheckShape(ShapeInput{Body: []byte(shapeBodySample)}, l); len(ms) != 0 {
		t.Fatalf("non-service token should pass: %v", ruleIDs(ms))
	}
	// Service token without a system message → allowed.
	body := `{"messages":[{"role":"user","content":"hi"}]}`
	if ms := CheckShape(ShapeInput{Body: []byte(body), IsServiceToken: true}, l); len(ms) != 0 {
		t.Fatalf("no system message should pass: %v", ruleIDs(ms))
	}
}

func TestShapeMultipleViolations(t *testing.T) {
	l := ShapeLimits{MaxMessages: 1, MaxBodyBytes: 5, MaxImageParts: 1, MaxTools: 1, DenySystemFromServiceTokens: true}
	ms := CheckShape(ShapeInput{Body: []byte(shapeBodySample), IsServiceToken: true}, l)
	if len(ms) != 5 {
		t.Fatalf("expected 5 violations, got %v", ruleIDs(ms))
	}
}

func TestShapeNonJSON(t *testing.T) {
	l := ShapeLimits{MaxMessages: 1, MaxBodyBytes: 5, MaxImageParts: 1, MaxTools: 1, DenySystemFromServiceTokens: true}
	ms := CheckShape(ShapeInput{Body: []byte("this is not json at all"), IsServiceToken: true}, l)
	if len(ms) != 1 || ms[0].RuleID != "max_body_bytes" {
		t.Fatalf("got %v", ruleIDs(ms))
	}
	l.MaxBodyBytes = 0
	if ms := CheckShape(ShapeInput{Body: []byte("nope"), IsServiceToken: true}, l); len(ms) != 0 {
		t.Fatalf("got %v", ruleIDs(ms))
	}
}

func TestShapeZeroLimitsNoop(t *testing.T) {
	ms := CheckShape(ShapeInput{Body: []byte(shapeBodySample), IsServiceToken: true}, ShapeLimits{})
	if len(ms) != 0 {
		t.Fatalf("zero limits must be unlimited, got %v", ruleIDs(ms))
	}
	if ms := CheckShape(ShapeInput{}, ShapeLimits{MaxMessages: 1, MaxBodyBytes: 1}); len(ms) != 0 {
		t.Fatalf("empty body: %v", ruleIDs(ms))
	}
}
