package adapter

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

func init() { Register(&TEI{}) }

// TEI is Hugging Face Text Embeddings Inference: the encoder-model server
// that hosts embedding models, rerankers and sequence classifiers — and, in
// practice, the standard way to serve Llama Prompt Guard 2 (a DeBERTa
// encoder that generative engines such as vLLM cannot load at all).
//
// TEI serves exactly ONE model per process and exposes no /v1/models, so
// discovery reads /info instead and synthesises a single catalog entry from
// it. The /info payload also says WHAT the model is — an embedding model, a
// reranker, or a classifier with a label set — which lets discovery hand the
// gateway a classifier-role hint and the encoder's real input cap rather
// than making an admin guess both.
//
// TEI has no chat surface. A TEI upstream therefore never proxies a
// completion: Prepare only forwards the OpenAI-compatible /v1/embeddings
// path, which is the one TEI implements.
type TEI struct{}

// Type returns the stored adapter enum value.
func (a *TEI) Type() string { return "tei" }

// teiInfo is the subset of GET /info that discovery consumes.
type teiInfo struct {
	ModelID         string `json:"model_id"`
	ServedModelName string `json:"served_model_name"`
	MaxInputLength  int64  `json:"max_input_length"`
	ModelType       struct {
		Classifier *struct {
			ID2Label map[string]string `json:"id2label"`
		} `json:"classifier"`
		Embedding *json.RawMessage `json:"embedding"`
		Reranker  *json.RawMessage `json:"reranker"`
	} `json:"model_type"`
}

// Discover synthesises the single served model from /info.
func (a *TEI) Discover(ctx context.Context, up Upstream, client *http.Client) ([]DiscoveredModel, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, joinURL(up.BaseURL, "/info"), nil)
	if err != nil {
		return nil, fmt.Errorf("build discovery request: %w", err)
	}
	if up.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+up.APIKey)
	}
	body, err := doDiscovery(client, req)
	if err != nil {
		return nil, err
	}
	var info teiInfo
	if err := json.Unmarshal(body, &info); err != nil {
		return nil, fmt.Errorf("parse TEI /info: %w", err)
	}
	name := teiModelName(info)
	if name == "" {
		return nil, fmt.Errorf("TEI /info carries no model name")
	}
	m := DiscoveredModel{Name: name, ContextWindow: info.MaxInputLength}
	switch {
	case info.ModelType.Classifier != nil:
		// A sequence classifier is a guard candidate. The role hint is the
		// wire protocol the security gateway will speak to it.
		m.Modalities = []string{ModalityModeration}
		m.ClassifierRole = ClassifierRoleTextClassification
	case info.ModelType.Reranker != nil:
		m.Modalities = []string{ModalityEmbedding}
	default:
		m.Modalities = []string{ModalityEmbedding}
	}
	return []DiscoveredModel{m}, nil
}

// teiModelName prefers served_model_name and falls back to the trailing path
// element of model_id, because TEI started from a local directory reports
// "/models/Llama-Prompt-Guard-2-86M" in both fields and a catalog name that
// is a filesystem path is not something an admin should have to read.
func teiModelName(info teiInfo) string {
	for _, cand := range []string{info.ServedModelName, info.ModelID} {
		cand = strings.TrimRight(strings.TrimSpace(cand), "/")
		if cand == "" {
			continue
		}
		if i := strings.LastIndex(cand, "/"); i >= 0 {
			cand = cand[i+1:]
		}
		if cand != "" {
			return cand
		}
	}
	return ""
}

// Prepare forwards only the embeddings path, the one OpenAI-compatible
// endpoint TEI serves. Anything else is refused up front so a misrouted chat
// call fails with a clear reason instead of a 404 from the upstream that the
// caller cannot interpret.
func (a *TEI) Prepare(up Upstream, req *Request) (*Prepared, error) {
	if req.Path != "/v1/embeddings" {
		return nil, fmt.Errorf("TEI upstream %q serves /v1/embeddings only; %s is not supported", up.Name, req.Path)
	}
	header := cloneHeader(req.Header)
	if up.APIKey != "" {
		header.Set("Authorization", "Bearer "+up.APIKey)
	}
	return &Prepared{URL: joinURL(up.BaseURL, req.Path), Header: header, Body: req.Body}, nil
}

// ExtractUsage reads the OpenAI usage block TEI attaches to /v1/embeddings.
func (a *TEI) ExtractUsage(body []byte) (Usage, bool) { return parseOpenAIUsage(body) }

// NewStreamCollector: TEI never streams.
func (a *TEI) NewStreamCollector() StreamCollector { return &openAIStreamCollector{} }

// TransformResponse is a pass-through: /v1/embeddings is already OpenAI-shaped.
func (a *TEI) TransformResponse(req *Request, body []byte) ([]byte, error) {
	return PassthroughResponse(req, body)
}

// NewStreamTransformer is a pass-through for the same reason.
func (a *TEI) NewStreamTransformer(*Request, http.Header) StreamTransformer {
	return PassthroughStream{}
}
