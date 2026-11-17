// Package endpoints says what each path under /v1 is: a stateless inference
// call, an operation on a stored provider object the gateway tracks an owner
// for, or something the gateway does not offer.
//
// Every caller of one upstream shares that upstream's provider key, so a path
// the gateway forwards blindly lets any caller list, read or delete what
// another caller stored at the provider. Nothing is forwarded unless it is
// classified here.
package endpoints

import (
	"net/url"
	"strings"
)

// Class is the kind of endpoint a path names.
type Class int

// Endpoint classes.
const (
	// Refused paths are never forwarded.
	Refused Class = iota
	// Inference calls keep nothing at the provider that another call can reach.
	Inference
	// Tracked calls create or act on a stored object with a recorded owner.
	Tracked
)

// Op is what a tracked call does to its object.
type Op int

// Tracked operations.
const (
	OpNone Op = iota
	// OpCreate makes a new object; its id is in the response.
	OpCreate
	// OpList lists objects of a family; the response is filtered to the caller's.
	OpList
	// OpGet reads one object or something under it.
	OpGet
	// OpUpdate changes one object or adds something under it.
	OpUpdate
	// OpDelete removes one object.
	OpDelete
	// OpDerive acts on an owned object and answers with a new object of the
	// same or another family (a video remix, a finished upload's file).
	OpDerive
)

// Families of stored objects.
const (
	FamilyVideo        = "video"
	FamilyFile         = "file"
	FamilyUpload       = "upload"
	FamilyResponse     = "response"
	FamilyConversation = "conversation"
	FamilyVectorStore  = "vector_store"
	// Batch jobs and fine-tuning jobs run at the provider after the call
	// returns; the gateway meters them when they finish.
	FamilyBatch    = "batch"
	FamilyFineTune = "fine_tune"
)

// IsJob reports whether a family is work the provider finishes later, which
// the gateway must follow up to meter.
func IsJob(family string) bool { return family == FamilyBatch || family == FamilyFineTune }

// BatchEndpoint reports whether a batch may target the given endpoint: the
// inference calls whose usage the gateway can read back from a batch's output.
func BatchEndpoint(path string) bool {
	switch path {
	case "/v1/chat/completions", "/v1/completions", "/v1/responses", "/v1/embeddings", "/v1/moderations":
		return true
	}
	return false
}

// Endpoint is the classification of one request.
type Endpoint struct {
	Class  Class
	Family string
	Op     Op
	// ObjectID is the stored object the path names (item operations).
	ObjectID string
	// Creates is the family of the stored object a successful call answers
	// with ("" when it creates none). An inference call can create one too:
	// POST /v1/responses stores the response it returns.
	Creates string
	// Why explains a refusal to the caller.
	Why string
}

// NeedsOwnedObject reports whether the path names an object the caller must own.
func (e Endpoint) NeedsOwnedObject() bool { return e.Class == Tracked && e.ObjectID != "" }

// stateless are the inference paths (relative to /v1/), all POST.
var stateless = map[string]bool{
	"chat/completions": true, "completions": true,
	"responses": true, "responses/input_tokens": true, "responses/compact": true,
	"messages": true, "messages/count_tokens": true,
	"embeddings":         true,
	"images/generations": true, "images/edits": true, "images/variations": true,
	"audio/speech": true, "audio/transcriptions": true, "audio/translations": true,
	"moderations": true,
	// Self-hosted engines (vLLM, llama.cpp, TEI, SGLang).
	"rerank": true, "score": true, "pooling": true, "classify": true,
	"tokenize": true, "detokenize": true, "infill": true,
}

// notOffered names families the gateway refuses, with the reason shown.
var notOffered = map[string]string{
	"assistants":    "the Assistants API is not available through this gateway",
	"threads":       "the Assistants API is not available through this gateway",
	"realtime":      "realtime sessions hand out a provider credential that would bypass this gateway",
	"containers":    "provider containers are not available through this gateway",
	"evals":         "evals are not available through this gateway",
	"organization":  "provider account administration is not available through this gateway",
	"organizations": "provider account administration is not available through this gateway",
	"chatkit":       "ChatKit sessions are not available through this gateway",
	"skills":        "provider skills are not available through this gateway",
	"models":        "list the models you can use with GET /v1/models",
}

// CleanPath reports whether a request path is safe to classify and forward:
// no dot segments, no empty segments, and nothing encoded that a second
// decoder could turn into a path separator, a query or a different segment.
func CleanPath(u *url.URL) bool {
	raw := strings.ToLower(u.EscapedPath())
	for _, bad := range []string{"%2f", "%5c", "%3f", "%23", "%25", "%00", "%2e", "\\"} {
		if strings.Contains(raw, bad) {
			return false
		}
	}
	path := u.Path
	if !strings.HasPrefix(path, "/") || strings.ContainsAny(path, "\\?#\x00") {
		return false
	}
	segs := strings.Split(strings.TrimPrefix(path, "/"), "/")
	for i, seg := range segs {
		if seg == "." || seg == ".." {
			return false
		}
		if seg == "" && i != len(segs)-1 { // one trailing slash is tolerated
			return false
		}
		for _, r := range seg {
			if r < 0x20 || r == 0x7f {
				return false
			}
		}
	}
	return true
}

// Classify says what a request under /v1/ is. path is the decoded URL path.
func Classify(method, path string) Endpoint {
	rest, ok := strings.CutPrefix(path, "/v1/")
	if !ok {
		return refused("")
	}
	rest = strings.TrimSuffix(rest, "/")
	if rest == "" {
		return refused("")
	}
	if stateless[rest] {
		if method != "POST" {
			return refused("this endpoint takes POST requests")
		}
		e := Endpoint{Class: Inference}
		if rest == "responses" {
			e.Family, e.Creates = FamilyResponse, FamilyResponse
		}
		return e
	}
	segs := strings.Split(rest, "/")
	if why, no := notOffered[segs[0]]; no {
		return refused(why)
	}
	switch segs[0] {
	case "videos":
		return classifyVideos(method, segs[1:])
	case "files":
		return classifyFiles(method, segs[1:])
	case "uploads":
		return classifyUploads(method, segs[1:])
	case "responses":
		return classifyResponses(method, segs[1:])
	case "conversations":
		return classifyConversations(method, segs[1:])
	case "vector_stores":
		return classifyVectorStores(method, segs[1:])
	case "batches":
		return classifyBatches(method, segs[1:])
	case "fine_tuning":
		return classifyFineTuning(method, segs[1:])
	case "chat":
		if len(segs) >= 2 && segs[1] == "completions" {
			return refused("stored chat completions are not available through this gateway; the completion itself is returned by POST /v1/chat/completions")
		}
	}
	return refused("")
}

func refused(why string) Endpoint { return Endpoint{Class: Refused, Why: why} }

func tracked(family string, op Op, id string) Endpoint {
	e := Endpoint{Class: Tracked, Family: family, Op: op, ObjectID: id}
	if op == OpCreate {
		e.Creates = family
	}
	return e
}

// collection classifies the two calls every family has on its collection path.
func collection(method, family string) Endpoint {
	switch method {
	case "POST":
		return tracked(family, OpCreate, "")
	case "GET":
		return tracked(family, OpList, "")
	}
	return refused("")
}

// item classifies the calls on one object's own path.
func item(method, family, id string, update bool) Endpoint {
	switch method {
	case "GET":
		return tracked(family, OpGet, id)
	case "DELETE":
		return tracked(family, OpDelete, id)
	case "POST":
		if update {
			return tracked(family, OpUpdate, id)
		}
	}
	return refused("")
}

// classifyVideos covers the three provider shapes:
//
//	OpenAI: POST /videos -> id "video_…"; GET /videos/{id}[/content]; POST /videos/{id}/remix
//	xAI:    POST /videos/generations -> request_id (a UUID); GET /videos/{request_id}
//	Gemini: POST /videos -> id "models/<model>/operations/<op>"; GET /videos/<that id>[/content]
func classifyVideos(method string, segs []string) Endpoint {
	switch {
	case len(segs) == 0:
		return collection(method, FamilyVideo)
	case len(segs) == 1 && (segs[0] == "generations" || segs[0] == "edits" || segs[0] == "extensions"):
		if method == "POST" {
			return tracked(FamilyVideo, OpCreate, "")
		}
		return refused("")
	}
	// A Gemini operation name spans four segments.
	idLen := 1
	if len(segs) >= 4 && segs[0] == "models" && segs[2] == "operations" {
		idLen = 4
	}
	id := strings.Join(segs[:idLen], "/")
	tail := segs[idLen:]
	switch {
	case len(tail) == 0:
		return item(method, FamilyVideo, id, false)
	case len(tail) == 1 && tail[0] == "content" && method == "GET":
		return tracked(FamilyVideo, OpGet, id)
	case len(tail) == 1 && (tail[0] == "remix" || tail[0] == "extend" || tail[0] == "edits") && method == "POST":
		e := tracked(FamilyVideo, OpDerive, id)
		e.Creates = FamilyVideo
		return e
	}
	return refused("")
}

func classifyFiles(method string, segs []string) Endpoint {
	switch {
	case len(segs) == 0:
		return collection(method, FamilyFile)
	case len(segs) == 1:
		return item(method, FamilyFile, segs[0], false)
	case len(segs) == 2 && segs[1] == "content" && method == "GET":
		return tracked(FamilyFile, OpGet, segs[0])
	}
	return refused("")
}

func classifyUploads(method string, segs []string) Endpoint {
	switch {
	case len(segs) == 0 && method == "POST":
		return tracked(FamilyUpload, OpCreate, "")
	case len(segs) == 2 && method == "POST" && (segs[1] == "parts" || segs[1] == "cancel"):
		return tracked(FamilyUpload, OpUpdate, segs[0])
	case len(segs) == 2 && method == "POST" && segs[1] == "complete":
		e := tracked(FamilyUpload, OpDerive, segs[0])
		e.Creates = FamilyFile
		return e
	}
	return refused("")
}

// classifyResponses covers stored responses. POST /responses itself is an
// inference call (the stateless table); the response it stores is recorded
// from its reply.
func classifyResponses(method string, segs []string) Endpoint {
	switch {
	case len(segs) == 1:
		return item(method, FamilyResponse, segs[0], false)
	case len(segs) == 2 && segs[1] == "input_items" && method == "GET":
		return tracked(FamilyResponse, OpGet, segs[0])
	case len(segs) == 2 && segs[1] == "cancel" && method == "POST":
		return tracked(FamilyResponse, OpUpdate, segs[0])
	}
	return refused("")
}

func classifyConversations(method string, segs []string) Endpoint {
	switch {
	case len(segs) == 0 && method == "POST":
		return tracked(FamilyConversation, OpCreate, "")
	case len(segs) == 1:
		return item(method, FamilyConversation, segs[0], true)
	case len(segs) == 2 && segs[1] == "items":
		switch method {
		case "GET":
			return tracked(FamilyConversation, OpGet, segs[0])
		case "POST":
			return tracked(FamilyConversation, OpUpdate, segs[0])
		}
	case len(segs) == 3 && segs[1] == "items":
		switch method {
		case "GET":
			return tracked(FamilyConversation, OpGet, segs[0])
		case "DELETE":
			return tracked(FamilyConversation, OpUpdate, segs[0])
		}
	}
	return refused("")
}

func classifyVectorStores(method string, segs []string) Endpoint {
	switch {
	case len(segs) == 0:
		return collection(method, FamilyVectorStore)
	case len(segs) == 1:
		return item(method, FamilyVectorStore, segs[0], true)
	case len(segs) == 2 && segs[1] == "search" && method == "POST":
		return tracked(FamilyVectorStore, OpGet, segs[0])
	case len(segs) >= 2 && len(segs) <= 5 && (segs[1] == "files" || segs[1] == "file_batches"):
		// Everything under an owned store: its files, their content, batches.
		switch method {
		case "GET":
			return tracked(FamilyVectorStore, OpGet, segs[0])
		case "POST", "DELETE":
			return tracked(FamilyVectorStore, OpUpdate, segs[0])
		}
	}
	return refused("")
}

func classifyBatches(method string, segs []string) Endpoint {
	switch {
	case len(segs) == 0:
		return collection(method, FamilyBatch)
	case len(segs) == 1 && method == "GET":
		return tracked(FamilyBatch, OpGet, segs[0])
	case len(segs) == 2 && segs[1] == "cancel" && method == "POST":
		return tracked(FamilyBatch, OpUpdate, segs[0])
	}
	return refused("")
}

// classifyFineTuning covers fine-tuning jobs. Checkpoint permissions share a
// tuned model with other provider organizations and stay refused.
func classifyFineTuning(method string, segs []string) Endpoint {
	if len(segs) == 0 || segs[0] != "jobs" {
		return refused("only fine-tuning jobs are available through this gateway")
	}
	segs = segs[1:]
	switch {
	case len(segs) == 0:
		return collection(method, FamilyFineTune)
	case len(segs) == 1 && method == "GET":
		return tracked(FamilyFineTune, OpGet, segs[0])
	case len(segs) == 2 && method == "GET" && (segs[1] == "events" || segs[1] == "checkpoints"):
		return tracked(FamilyFineTune, OpGet, segs[0])
	case len(segs) == 2 && method == "POST" && (segs[1] == "cancel" || segs[1] == "pause" || segs[1] == "resume"):
		return tracked(FamilyFineTune, OpUpdate, segs[0])
	}
	return refused("")
}
