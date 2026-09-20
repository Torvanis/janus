package classify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// ProtocolTextClassification is the HF Text Embeddings Inference
// sequence-classification wire shape, verified against TEI 1.9.4:
//
//	POST {base}/predict   {"inputs": [["..."], ["..."]]}
//	→ [[{"label":"MALICIOUS","score":0.98},{"label":"BENIGN","score":0.02}], ...]
//
// The batch MUST be nested one-string-per-inner-array. TEI's PredictInput
// parses a flat ["a","b"] as a single cross-encoder PAIR (text + context)
// and returns ONE verdict for the two texts — a silent misclassification, not
// an error. A single input may be sent as a bare string and comes back as a
// single inner array; both response shapes are accepted.
//
// This is how Llama Prompt Guard 2 is served: it is a DeBERTa encoder that
// generative engines (vLLM) cannot load, so TEI is the standard host.
const ProtocolTextClassification = "text_classification"

func init() {
	Register(ProtocolTextClassification, newTextClassification)
}

type textClassification struct {
	up Upstream
	// maliciousLabels are the labels the backend maps to Malicious=true.
	// Prompt Guard 2 emits MALICIOUS / BENIGN; Prompt Guard 1 emitted
	// JAILBREAK / INJECTION / BENIGN. Compared case-insensitively.
	maliciousLabels map[string]bool
}

func newTextClassification(up Upstream) (Classifier, error) {
	if strings.TrimSpace(up.BaseURL) == "" {
		return nil, fmt.Errorf("classifier %s has no base URL", up.ModelName)
	}
	if up.Client == nil {
		up.Client = http.DefaultClient
	}
	return &textClassification{up: up, maliciousLabels: map[string]bool{
		"malicious": true, "jailbreak": true, "injection": true, "unsafe": true, "label_1": true,
	}}, nil
}

func (t *textClassification) Protocol() string { return ProtocolTextClassification }

func (t *textClassification) Classify(ctx context.Context, in []Segment) ([]Score, error) {
	if len(in) == 0 {
		return nil, nil
	}
	// Nested batch: each text in its own inner array. See the protocol doc
	// for why a flat list is wrong (TEI reads it as one cross-encoder pair).
	inputs := make([][]string, len(in))
	chars := 0
	for i, s := range in {
		inputs[i] = []string{s.Text}
		chars += len(s.Text)
	}
	payload, _ := json.Marshal(map[string]any{"inputs": inputs})
	url := strings.TrimRight(t.up.BaseURL, "/") + "/predict"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if t.up.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+t.up.APIKey)
	}
	started := time.Now()
	resp, err := t.up.Client.Do(req)
	if t.up.Meter != nil {
		defer func() { t.up.Meter(ctx, time.Since(started), chars, err) }()
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if readErr != nil {
		err = readErr
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		err = fmt.Errorf("classifier returned HTTP %d: %s", resp.StatusCode, truncate(string(body), 200))
		return nil, err
	}
	scores, perr := parseTextClassification(body, len(in))
	if perr != nil {
		err = perr
		return nil, err
	}
	out := make([]Score, 0, len(in))
	for i, labels := range scores {
		if len(labels) == 0 {
			// A verdict with no labels is not "benign", it is a broken
			// classifier. Fail loudly so the check's fail stance decides.
			err = fmt.Errorf("classifier returned no labels for input %d", i)
			return nil, err
		}
		// The verdict is the argmax label. Malicious is true only when the
		// top label is one of the attack labels; the numeric threshold is
		// the engine's business, so a 0.1 MALICIOUS under a 0.9 BENIGN is
		// reported as benign 0.9, not malicious 0.1.
		best := Score{Index: in[i].Index, Label: "BENIGN"}
		for _, l := range labels {
			if l.Score > best.Score {
				best = Score{Index: in[i].Index, Label: l.Label, Score: l.Score, Malicious: t.maliciousLabels[strings.ToLower(l.Label)]}
			}
		}
		out = append(out, best)
	}
	return out, nil
}

type labelScore struct {
	Label string  `json:"label"`
	Score float64 `json:"score"`
}

// parseTextClassification accepts [[{label,score}...]...] (batched) and
// [{label,score}...] (single) and returns one label list per input.
func parseTextClassification(body []byte, n int) ([][]labelScore, error) {
	var batched [][]labelScore
	if err := json.Unmarshal(body, &batched); err == nil {
		if len(batched) != n {
			return nil, fmt.Errorf("classifier returned %d results for %d inputs", len(batched), n)
		}
		return batched, nil
	}
	var single []labelScore
	if err := json.Unmarshal(body, &single); err == nil && n == 1 {
		return [][]labelScore{single}, nil
	}
	return nil, fmt.Errorf("classifier response is not a text-classification result: %s", truncate(string(body), 200))
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
