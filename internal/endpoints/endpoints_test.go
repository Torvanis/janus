package endpoints

import (
	"net/url"
	"testing"
)

func TestClassify(t *testing.T) {
	cases := []struct {
		method, path string
		class        Class
		family       string
		op           Op
		id, creates  string
	}{
		{"POST", "/v1/chat/completions", Inference, "", OpNone, "", ""},
		{"POST", "/v1/embeddings", Inference, "", OpNone, "", ""},
		{"POST", "/v1/messages", Inference, "", OpNone, "", ""},
		{"POST", "/v1/audio/transcriptions", Inference, "", OpNone, "", ""},
		{"POST", "/v1/responses", Inference, FamilyResponse, OpNone, "", FamilyResponse},
		{"GET", "/v1/chat/completions", Refused, "", OpNone, "", ""},
		{"GET", "/v1/chat/completions/chatcmpl-1", Refused, "", OpNone, "", ""},

		{"POST", "/v1/videos", Tracked, FamilyVideo, OpCreate, "", FamilyVideo},
		{"POST", "/v1/videos/generations", Tracked, FamilyVideo, OpCreate, "", FamilyVideo},
		{"GET", "/v1/videos", Tracked, FamilyVideo, OpList, "", ""},
		{"GET", "/v1/videos/video_1", Tracked, FamilyVideo, OpGet, "video_1", ""},
		{"GET", "/v1/videos/video_1/content", Tracked, FamilyVideo, OpGet, "video_1", ""},
		{"DELETE", "/v1/videos/video_1", Tracked, FamilyVideo, OpDelete, "video_1", ""},
		{"POST", "/v1/videos/video_1/remix", Tracked, FamilyVideo, OpDerive, "video_1", FamilyVideo},
		{"GET", "/v1/videos/models/veo-3/operations/abc", Tracked, FamilyVideo, OpGet, "models/veo-3/operations/abc", ""},
		{"GET", "/v1/videos/models/veo-3/operations/abc/content", Tracked, FamilyVideo, OpGet, "models/veo-3/operations/abc", ""},
		{"GET", "/v1/videos/a/b", Refused, "", OpNone, "", ""},
		{"PUT", "/v1/videos/video_1", Refused, "", OpNone, "", ""},

		{"POST", "/v1/files", Tracked, FamilyFile, OpCreate, "", FamilyFile},
		{"GET", "/v1/files", Tracked, FamilyFile, OpList, "", ""},
		{"GET", "/v1/files/file-1/content", Tracked, FamilyFile, OpGet, "file-1", ""},
		{"DELETE", "/v1/files/file-1", Tracked, FamilyFile, OpDelete, "file-1", ""},
		{"POST", "/v1/files/file-1", Refused, "", OpNone, "", ""},

		{"POST", "/v1/uploads", Tracked, FamilyUpload, OpCreate, "", FamilyUpload},
		{"POST", "/v1/uploads/upload_1/parts", Tracked, FamilyUpload, OpUpdate, "upload_1", ""},
		{"POST", "/v1/uploads/upload_1/complete", Tracked, FamilyUpload, OpDerive, "upload_1", FamilyFile},

		{"GET", "/v1/responses/resp_1", Tracked, FamilyResponse, OpGet, "resp_1", ""},
		{"DELETE", "/v1/responses/resp_1", Tracked, FamilyResponse, OpDelete, "resp_1", ""},
		{"GET", "/v1/responses/resp_1/input_items", Tracked, FamilyResponse, OpGet, "resp_1", ""},
		{"POST", "/v1/responses/resp_1/cancel", Tracked, FamilyResponse, OpUpdate, "resp_1", ""},
		{"GET", "/v1/responses", Refused, "", OpNone, "", ""},

		{"POST", "/v1/conversations", Tracked, FamilyConversation, OpCreate, "", FamilyConversation},
		{"GET", "/v1/conversations/conv_1/items", Tracked, FamilyConversation, OpGet, "conv_1", ""},
		{"POST", "/v1/conversations/conv_1/items", Tracked, FamilyConversation, OpUpdate, "conv_1", ""},
		{"GET", "/v1/conversations", Refused, "", OpNone, "", ""},

		{"POST", "/v1/vector_stores", Tracked, FamilyVectorStore, OpCreate, "", FamilyVectorStore},
		{"GET", "/v1/vector_stores", Tracked, FamilyVectorStore, OpList, "", ""},
		{"POST", "/v1/vector_stores/vs_1/search", Tracked, FamilyVectorStore, OpGet, "vs_1", ""},
		{"POST", "/v1/vector_stores/vs_1/files", Tracked, FamilyVectorStore, OpUpdate, "vs_1", ""},
		{"GET", "/v1/vector_stores/vs_1/files/file-1/content", Tracked, FamilyVectorStore, OpGet, "vs_1", ""},

		{"POST", "/v1/batches", Tracked, FamilyBatch, OpCreate, "", FamilyBatch},
		{"GET", "/v1/batches", Tracked, FamilyBatch, OpList, "", ""},
		{"GET", "/v1/batches/batch_1", Tracked, FamilyBatch, OpGet, "batch_1", ""},
		{"POST", "/v1/batches/batch_1/cancel", Tracked, FamilyBatch, OpUpdate, "batch_1", ""},
		{"DELETE", "/v1/batches/batch_1", Refused, "", OpNone, "", ""},
		{"POST", "/v1/fine_tuning/jobs", Tracked, FamilyFineTune, OpCreate, "", FamilyFineTune},
		{"GET", "/v1/fine_tuning/jobs", Tracked, FamilyFineTune, OpList, "", ""},
		{"GET", "/v1/fine_tuning/jobs/ftjob-1/events", Tracked, FamilyFineTune, OpGet, "ftjob-1", ""},
		{"POST", "/v1/fine_tuning/jobs/ftjob-1/cancel", Tracked, FamilyFineTune, OpUpdate, "ftjob-1", ""},
		{"GET", "/v1/fine_tuning/checkpoints/ckpt-1/permissions", Refused, "", OpNone, "", ""},
		{"POST", "/v1/fine_tuning/alpha/graders/run", Refused, "", OpNone, "", ""},
		{"GET", "/v1/organization/users", Refused, "", OpNone, "", ""},
		{"POST", "/v1/realtime/client_secrets", Refused, "", OpNone, "", ""},
		{"GET", "/v1/assistants", Refused, "", OpNone, "", ""},
		{"GET", "/v1/models/gpt-4o", Refused, "", OpNone, "", ""},
		{"GET", "/v1/something-new", Refused, "", OpNone, "", ""},
		{"GET", "/v2/files", Refused, "", OpNone, "", ""},
	}
	for _, c := range cases {
		got := Classify(c.method, c.path)
		if got.Class != c.class || got.Family != c.family || got.Op != c.op || got.ObjectID != c.id || got.Creates != c.creates {
			t.Errorf("%s %s: got %+v", c.method, c.path, got)
		}
	}
	if Classify("GET", "/v1/realtime/sessions").Why == "" {
		t.Error("a refused family should say why")
	}
}

func TestCleanPath(t *testing.T) {
	for raw, want := range map[string]bool{
		"/v1/chat/completions":                   true,
		"/v1/videos/models/veo-3.1/operations/x": true,
		"/v1/files/":                             true,
		"/v1/files/../chat/completions":          false,
		"/v1/./files":                            false,
		"/v1//files":                             false,
		"/v1/files%2f..%2fbatches":               false,
		"/v1/files/file-1%3fx=1":                 false,
		"/v1/files/file-1%23frag":                false,
		"/v1/files/%2e%2e/batches":               false,
		"/v1/files/a%5cb":                        false,
		"/v1/files/a%00b":                        false,
		"/v1/files/a%2541":                       false,
	} {
		u, err := url.ParseRequestURI(raw)
		if err != nil {
			if want {
				t.Errorf("%s: %v", raw, err)
			}
			continue
		}
		if got := CleanPath(u); got != want {
			t.Errorf("CleanPath(%s) = %v, want %v", raw, got, want)
		}
	}
}
