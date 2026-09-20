package adapter

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// liveTEIInfo is the verbatim GET /info payload from TEI 1.9.4 serving
// Llama Prompt Guard 2 86M from a local directory. Kept literal so the test
// pins the real wire shape, not a guess at it.
const liveTEIInfo = `{"model_id":"/models/Llama-Prompt-Guard-2-86M","model_sha":null,"model_dtype":"float32",
"served_model_name":"/models/Llama-Prompt-Guard-2-86M",
"model_type":{"classifier":{"id2label":{"0":"BENIGN","1":"MALICIOUS"},"label2id":{"MALICIOUS":1,"BENIGN":0}}},
"max_concurrent_requests":512,"max_input_length":512,"max_batch_tokens":16384,"max_batch_requests":4,
"max_client_batch_size":64,"auto_truncate":true,"tokenization_workers":3,"version":"1.9.4"}`

func TestTEIDiscoverSynthesisesClassifierFromInfo(t *testing.T) {
	a := &TEI{}
	var gotPath, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(liveTEIInfo))
	}))
	defer srv.Close()

	models, err := a.Discover(context.Background(), Upstream{BaseURL: srv.URL + "/", APIKey: "k"}, srv.Client())
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if gotPath != "/info" {
		t.Errorf("discovery path = %q, want /info (TEI has no /v1/models)", gotPath)
	}
	if gotAuth != "Bearer k" {
		t.Errorf("Authorization = %q", gotAuth)
	}
	if len(models) != 1 {
		t.Fatalf("TEI serves one model per process; discovered %d: %+v", len(models), models)
	}
	m := models[0]
	if m.Name != "Llama-Prompt-Guard-2-86M" {
		t.Errorf("name = %q, want the trailing path element, not the filesystem path", m.Name)
	}
	if m.ClassifierRole != ClassifierRoleTextClassification {
		t.Errorf("classifier role = %q, want %q from model_type.classifier", m.ClassifierRole, ClassifierRoleTextClassification)
	}
	if m.ContextWindow != 512 {
		t.Errorf("context window = %d, want max_input_length 512", m.ContextWindow)
	}
	if len(m.Modalities) != 1 || m.Modalities[0] != ModalityModeration {
		t.Errorf("modalities = %v, want [%s]", m.Modalities, ModalityModeration)
	}
}

func TestTEIDiscoverEmbeddingModelGetsNoRole(t *testing.T) {
	a := &TEI{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"model_id":"BAAI/bge-base-en-v1.5","served_model_name":"bge-base",
			"model_type":{"embedding":{"pooling":"cls"}},"max_input_length":512}`))
	}))
	defer srv.Close()
	models, err := a.Discover(context.Background(), Upstream{BaseURL: srv.URL}, srv.Client())
	if err != nil || len(models) != 1 {
		t.Fatalf("Discover: %v %+v", err, models)
	}
	if models[0].Name != "bge-base" {
		t.Errorf("served_model_name must win over model_id: got %q", models[0].Name)
	}
	if models[0].ClassifierRole != "" {
		t.Errorf("an embedding model must not be offered as a classifier: role=%q", models[0].ClassifierRole)
	}
	if len(models[0].Modalities) != 1 || models[0].Modalities[0] != ModalityEmbedding {
		t.Errorf("modalities = %v", models[0].Modalities)
	}
}

func TestTEIDiscoverRejectsNamelessInfo(t *testing.T) {
	a := &TEI{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"model_type":{"classifier":{"id2label":{"0":"A"}}}}`))
	}))
	defer srv.Close()
	if _, err := a.Discover(context.Background(), Upstream{BaseURL: srv.URL}, srv.Client()); err == nil {
		t.Fatal("an /info with no model name must fail discovery, not create an unnamed catalog row")
	}
}

func TestTEIPrepareRefusesNonEmbeddingPaths(t *testing.T) {
	a := &TEI{}
	up := Upstream{Name: "pg", BaseURL: "http://tei"}
	if _, err := a.Prepare(up, &Request{Path: "/v1/chat/completions", Header: http.Header{}}); err == nil ||
		!strings.Contains(err.Error(), "/v1/embeddings only") {
		t.Fatalf("chat completion against TEI must be refused with a clear reason, got %v", err)
	}
	p, err := a.Prepare(Upstream{Name: "e", BaseURL: "http://tei/", APIKey: "k"}, &Request{Path: "/v1/embeddings", Header: http.Header{}})
	if err != nil {
		t.Fatal(err)
	}
	if p.URL != "http://tei/v1/embeddings" || p.Header.Get("Authorization") != "Bearer k" {
		t.Errorf("prepared = %s %v", p.URL, p.Header)
	}
}
