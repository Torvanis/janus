// Package blobstoretest provides an in-memory S3-compatible server for
// tests that exercise the S3 file storage backend without a real service.
// It speaks just enough of the path-style S3 REST API (HeadBucket,
// CreateBucket, PutObject, GetObject, DeleteObject) for internal/blobstore
// and does not check signatures.
package blobstoretest

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// FakeS3 is a running in-memory S3 server.
type FakeS3 struct {
	*httptest.Server

	mu      sync.Mutex
	buckets map[string]map[string][]byte
}

// NewFakeS3 starts a server that is closed when the test ends.
func NewFakeS3(t testing.TB) *FakeS3 {
	f := &FakeS3{buckets: map[string]map[string][]byte{}}
	f.Server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.Close)
	return f
}

// Objects returns a copy of every object in a bucket, keyed by object key.
func (f *FakeS3) Objects(bucket string) map[string][]byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string][]byte{}
	for k, v := range f.buckets[bucket] {
		out[k] = append([]byte(nil), v...)
	}
	return out
}

func s3Error(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, `<?xml version="1.0" encoding="UTF-8"?><Error><Code>`+code+`</Code><Message>`+code+`</Message></Error>`)
}

func (f *FakeS3) serve(w http.ResponseWriter, r *http.Request) {
	bucket, key, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
	f.mu.Lock()
	defer f.mu.Unlock()
	objs, exists := f.buckets[bucket]
	if key == "" {
		switch r.Method {
		case http.MethodHead:
			if !exists {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.WriteHeader(http.StatusOK)
		case http.MethodPut:
			if exists {
				s3Error(w, http.StatusConflict, "BucketAlreadyOwnedByYou")
				return
			}
			f.buckets[bucket] = map[string][]byte{}
			w.WriteHeader(http.StatusOK)
		default:
			s3Error(w, http.StatusMethodNotAllowed, "MethodNotAllowed")
		}
		return
	}
	if !exists {
		s3Error(w, http.StatusNotFound, "NoSuchBucket")
		return
	}
	switch r.Method {
	case http.MethodPut:
		body, err := io.ReadAll(r.Body)
		if err != nil {
			s3Error(w, http.StatusBadRequest, "IncompleteBody")
			return
		}
		objs[key] = body
		w.Header().Set("ETag", `"fake"`)
		w.WriteHeader(http.StatusOK)
	case http.MethodGet:
		data, ok := objs[key]
		if !ok {
			s3Error(w, http.StatusNotFound, "NoSuchKey")
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(data)
	case http.MethodDelete:
		delete(objs, key)
		w.WriteHeader(http.StatusNoContent)
	default:
		s3Error(w, http.StatusMethodNotAllowed, "MethodNotAllowed")
	}
}
