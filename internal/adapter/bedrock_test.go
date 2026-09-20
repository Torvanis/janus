package adapter

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestSignAWSV4GoldenSignature pins the SigV4 output for a fixed request. The
// expected values were computed with an independent implementation of the AWS
// Signature Version 4 specification (not with this package), so any drift in
// the signing code from the spec fails this test.
func TestSignAWSV4GoldenSignature(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":[{"text":"hi"}]}]}`)
	req, err := http.NewRequest(http.MethodPost,
		"https://bedrock-runtime.us-east-1.amazonaws.com/model/anthropic.claude-3-haiku/converse", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	creds := awsCredentials{
		AccessKeyID:     "AKIDEXAMPLE",
		SecretAccessKey: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
	}
	now := time.Date(2024, 1, 15, 12, 30, 45, 0, time.UTC)

	if err := signAWSV4(req, body, creds, "us-east-1", "bedrock", now); err != nil {
		t.Fatalf("sign: %v", err)
	}

	if got := req.Header.Get("X-Amz-Date"); got != "20240115T123045Z" {
		t.Fatalf("X-Amz-Date = %q, want 20240115T123045Z", got)
	}
	const wantHash = "c3335e07d6e89650bf94cb98664be69f8774dbc3198df221f113db0a59ea14f0"
	if got := req.Header.Get("X-Amz-Content-Sha256"); got != wantHash {
		t.Fatalf("X-Amz-Content-Sha256 = %q, want %q", got, wantHash)
	}
	const wantAuth = "AWS4-HMAC-SHA256 " +
		"Credential=AKIDEXAMPLE/20240115/us-east-1/bedrock/aws4_request, " +
		"SignedHeaders=content-type;host;x-amz-content-sha256;x-amz-date, " +
		"Signature=be073e8c9feb84eba85d2d84d115faa8400cea6458b154104eb31c5f4aa37a02"
	if got := req.Header.Get("Authorization"); got != wantAuth {
		t.Fatalf("Authorization =\n  %q\nwant\n  %q", got, wantAuth)
	}
}

func TestSignAWSV4SessionTokenIsSigned(t *testing.T) {
	req, err := http.NewRequest(http.MethodPost, "https://bedrock-runtime.us-east-1.amazonaws.com/model/m/converse", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	creds := awsCredentials{AccessKeyID: "AKID", SecretAccessKey: "SECRET", SessionToken: "SESSION"}
	if err := signAWSV4(req, nil, creds, "us-east-1", "bedrock", time.Now().UTC()); err != nil {
		t.Fatalf("sign: %v", err)
	}
	if got := req.Header.Get("X-Amz-Security-Token"); got != "SESSION" {
		t.Fatalf("X-Amz-Security-Token = %q", got)
	}
	if auth := req.Header.Get("Authorization"); !strings.Contains(auth, "x-amz-security-token") {
		t.Fatalf("session token must be part of the signed headers, got %q", auth)
	}
}

func TestBedrockPrepare(t *testing.T) {
	a := &Bedrock{}
	up := Upstream{
		ID:      "up-bedrock",
		Name:    "aws",
		BaseURL: "https://bedrock-runtime.eu-west-1.amazonaws.com",
		APIKey:  "AKIDEXAMPLE:SECRETKEY",
	}
	hdr := http.Header{}
	hdr.Set("Authorization", "Bearer janus-downstream-token")
	hdr.Set("Content-Type", "application/json")
	req := &Request{
		Method: http.MethodPost,
		Path:   "/v1/chat/completions",
		Header: hdr,
		Model:  "meta.llama3-8b",
		Body: []byte(`{"model":"meta.llama3-8b","messages":[` +
			`{"role":"system","content":"be brief"},` +
			`{"role":"user","content":"hi"}],` +
			`"max_tokens":64,"temperature":0.5}`),
	}

	prep, err := a.Prepare(up, req)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if want := "https://bedrock-runtime.eu-west-1.amazonaws.com/model/meta.llama3-8b/converse"; prep.URL != want {
		t.Fatalf("URL = %q, want %q", prep.URL, want)
	}

	auth := prep.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/") {
		t.Fatalf("Authorization not SigV4: %q", auth)
	}
	if !strings.Contains(auth, "/eu-west-1/bedrock/aws4_request") {
		t.Fatalf("Authorization scope must use the region from the base URL: %q", auth)
	}
	if strings.Contains(auth, "janus-downstream-token") {
		t.Fatal("the downstream bearer token leaked into the upstream request")
	}

	sum := sha256.Sum256(prep.Body)
	if got := prep.Header.Get("X-Amz-Content-Sha256"); got != hex.EncodeToString(sum[:]) {
		t.Fatal("X-Amz-Content-Sha256 does not match the signed body")
	}

	var converse struct {
		Messages []struct {
			Role    string `json:"role"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"messages"`
		System []struct {
			Text string `json:"text"`
		} `json:"system"`
		InferenceConfig struct {
			MaxTokens   int     `json:"maxTokens"`
			Temperature float64 `json:"temperature"`
		} `json:"inferenceConfig"`
	}
	if err := json.Unmarshal(prep.Body, &converse); err != nil {
		t.Fatalf("prepared body is not valid JSON: %v", err)
	}
	if len(converse.Messages) != 1 || converse.Messages[0].Role != "user" ||
		len(converse.Messages[0].Content) != 1 || converse.Messages[0].Content[0].Text != "hi" {
		t.Fatalf("messages not in Converse shape: %s", prep.Body)
	}
	if len(converse.System) != 1 || converse.System[0].Text != "be brief" {
		t.Fatalf("system prompt not lifted to the top-level system field: %s", prep.Body)
	}
	if converse.InferenceConfig.MaxTokens != 64 || converse.InferenceConfig.Temperature != 0.5 {
		t.Fatalf("inferenceConfig not mapped: %s", prep.Body)
	}

	// Streaming targets the converse-stream action.
	req.Streaming = true
	prep, err = a.Prepare(up, req)
	if err != nil {
		t.Fatalf("Prepare (streaming): %v", err)
	}
	if !strings.HasSuffix(prep.URL, "/model/meta.llama3-8b/converse-stream") {
		t.Fatalf("streaming URL = %q, want …/converse-stream", prep.URL)
	}
}

func TestParseAWSCredentials(t *testing.T) {
	creds, err := parseAWSCredentials("AKID:SECRET")
	if err != nil {
		t.Fatalf("two-part credentials rejected: %v", err)
	}
	if creds.AccessKeyID != "AKID" || creds.SecretAccessKey != "SECRET" || creds.SessionToken != "" {
		t.Fatalf("parsed = %+v", creds)
	}

	creds, err = parseAWSCredentials("AKID:SECRET:token:with:colons")
	if err != nil {
		t.Fatalf("three-part credentials rejected: %v", err)
	}
	if creds.SessionToken != "token:with:colons" {
		t.Fatalf("session token = %q, want colons preserved", creds.SessionToken)
	}

	if _, err := parseAWSCredentials("just-a-key"); err == nil {
		t.Fatal("credentials without a secret must be rejected")
	}
	if _, err := parseAWSCredentials(":SECRET"); err == nil {
		t.Fatal("credentials with an empty access key must be rejected")
	}
}

func TestRegionFromHost(t *testing.T) {
	if got := regionFromHost("https://bedrock-runtime.us-west-2.amazonaws.com"); got != "us-west-2" {
		t.Fatalf("region = %q, want us-west-2", got)
	}
	if got := regionFromHost("https://example.com"); got != "us-east-1" {
		t.Fatalf("non-AWS host must default to us-east-1, got %q", got)
	}
}

func TestBedrockExtractUsage(t *testing.T) {
	a := &Bedrock{}
	u, ok := a.ExtractUsage([]byte(`{"stopReason":"end_turn","usage":{"inputTokens":12,"outputTokens":34,"cacheReadInputTokens":5}}`))
	if !ok || !u.Reported {
		t.Fatal("usage block must be reported")
	}
	if u.TokensIn != 12 || u.TokensOut != 34 || u.TokensCached != 5 || u.FinishReason != "end_turn" {
		t.Fatalf("usage = %+v", u)
	}

	if _, ok := a.ExtractUsage([]byte(`not json`)); ok {
		t.Fatal("invalid JSON must not report usage")
	}
	if _, ok := a.ExtractUsage([]byte(`{}`)); ok {
		t.Fatal("zero usage must not be reported")
	}
}

func TestBedrockStreamCollector(t *testing.T) {
	c := (&Bedrock{}).NewStreamCollector()
	// Bedrock metadata may arrive as a bare frame or as an SSE data line; the
	// collector accepts both.
	c.Feed([]byte(`{"metadata":true}`))
	c.Feed([]byte(`data: {"usage":{"inputTokens":10,"outputTokens":2}}`))
	c.Feed([]byte(`{"usage":{"inputTokens":10,"outputTokens":40},"stopReason":"max_tokens"}`))

	u := c.Usage()
	if !u.Reported {
		t.Fatal("streamed usage must be reported")
	}
	if u.TokensIn != 10 || u.TokensOut != 40 || u.FinishReason != "max_tokens" {
		t.Fatalf("usage = %+v", u)
	}
}
