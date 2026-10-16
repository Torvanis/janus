package httpapi

import (
	"encoding/json"
	"testing"
)

func TestAdjustRefusedParam(t *testing.T) {
	body := []byte(`{"model":"m","max_tokens":300,"temperature":0,"messages":[]}`)
	refusedMax := []byte(`{"error":{"message":"Unsupported parameter: 'max_tokens' is not supported with this model. Use 'max_completion_tokens' instead.","param":"max_tokens","code":"unsupported_parameter"}}`)
	out, ok := adjustRefusedParam(body, refusedMax)
	if !ok {
		t.Fatal("max_tokens refusal must be fixed")
	}
	var m map[string]any
	_ = json.Unmarshal(out, &m)
	if _, still := m["max_tokens"]; still || m["max_completion_tokens"] != float64(reasoningOutputFloor) {
		t.Fatalf("max_tokens must become max_completion_tokens with reasoning headroom: %s", out)
	}
	refusedTemp := []byte(`{"error":{"message":"Unsupported value: 'temperature' does not support 0 with this model. Only the default (1) value is supported.","param":"temperature"}}`)
	out, ok = adjustRefusedParam(out, refusedTemp)
	_ = json.Unmarshal(out, &m)
	m = map[string]any{}
	_ = json.Unmarshal(out, &m)
	if _, still := m["temperature"]; !ok || still {
		t.Fatalf("temperature refusal must drop temperature: %s", out)
	}
	if _, ok := adjustRefusedParam(body, []byte(`{"error":{"message":"context length exceeded","param":"messages"}}`)); ok {
		t.Fatal("unrelated 400s must not be retried")
	}
	if got := upstreamErrorText([]byte(`{"error":{"message":"bad\n  thing"}}`)); got != "bad thing" {
		t.Fatalf("error text %q", got)
	}
}

func TestNameAnswers(t *testing.T) {
	got := nameAnswers("B correctly handles input, while A adds tests; Answer A is longer. A/B testing aside, B wins.", "sonnet", "opus")
	// "A/B testing" is not a reference to either answer.
	want := "opus correctly handles input, while sonnet adds tests; sonnet is longer. A/B testing aside, opus wins."
	if got != want {
		t.Fatalf("got %q", got)
	}
	if got := nameAnswers("Both are fine; a tie.", "x", "y"); got != "Both are fine; a tie." {
		t.Fatalf("lowercase a must not be replaced: %q", got)
	}
}
