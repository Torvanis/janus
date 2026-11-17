package httpapi

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strings"

	"github.com/torvanis/janus/internal/endpoints"
	"github.com/torvanis/janus/internal/store"
)

// Stored provider objects and who may reach them. Every caller of an upstream
// shares that upstream's provider key, so the provider cannot tell callers
// apart: the gateway records who created each object (store.UpstreamObject)
// and lets only that owner read, change, delete, list or refer to it.
// internal/endpoints decides which paths exist at all.

// objectTrack is the stored-object side of one proxied request.
type objectTrack struct {
	ep        endpoints.Endpoint
	ownerType string
	ownerID   string
	// record is the caller's record of the object the path names. It pins the
	// model (and so the upstream) that serves the call.
	record *store.UpstreamObject
	// home routes a call that named no model: a list (where the caller's
	// objects live) or a batch (where its input file lives).
	home *store.UpstreamObject
	// detail is who to bill when a job this call starts has ended.
	detail *jobDetail
}

// pinned is the record that decides which model serves, if any.
func (t *objectTrack) pinned() *store.UpstreamObject {
	if t == nil {
		return nil
	}
	if t.record != nil {
		return t.record
	}
	return t.home
}

func objectOwner(user *store.User, serviceToken *store.ServiceToken) (string, string) {
	if serviceToken != nil {
		return store.ObjectOwnerServiceToken, serviceToken.ID
	}
	return store.ObjectOwnerUser, user.ID
}

// familyLabel names a family in an error message.
func familyLabel(family string) string {
	switch family {
	case endpoints.FamilyVideo:
		return "video"
	case endpoints.FamilyFile:
		return "file"
	case endpoints.FamilyUpload:
		return "upload"
	case endpoints.FamilyResponse:
		return "response"
	case endpoints.FamilyConversation:
		return "conversation"
	case endpoints.FamilyVectorStore:
		return "vector store"
	case endpoints.FamilyBatch:
		return "batch"
	case endpoints.FamilyFineTune:
		return "fine-tuning job"
	}
	return "object"
}

// errEndpointNotOffered refuses a path the gateway does not forward.
func errEndpointNotOffered(method, path, why string) *APIError {
	msg := method + " " + truncate(path, 120) + " is not available through this gateway"
	if why != "" {
		msg += ": " + why
	}
	return newError(http.StatusNotFound, CodeNotFound, "invalid_request_error", msg+".")
}

// errObjectNotFound answers for an object the caller does not own. An id that
// was never recorded, one that belongs to someone else and one the owner
// deleted all read the same, so an id cannot be confirmed by probing.
func errObjectNotFound(family string) *APIError {
	return newError(http.StatusNotFound, CodeNotFound, "invalid_request_error",
		"That "+familyLabel(family)+" was not found. Only objects you created through this gateway are available.")
}

// emptyList is the answer to a list call from a caller who owns nothing of
// that family: true, and it needs no provider call.
const emptyList = `{"object":"list","data":[],"first_id":null,"last_id":null,"has_more":false}`

// --- ids in responses ---------------------------------------------------------

// createdObjectID reads the id of the object a successful create answered with.
func createdObjectID(ep endpoints.Endpoint, payload []byte) string {
	var doc struct {
		ID        string `json:"id"`
		RequestID string `json:"request_id"`
		File      struct {
			ID string `json:"id"`
		} `json:"file"`
	}
	if json.Unmarshal(payload, &doc) != nil {
		return ""
	}
	if ep.Family == endpoints.FamilyUpload && ep.Creates == endpoints.FamilyFile {
		return doc.File.ID
	}
	if doc.ID != "" {
		return doc.ID
	}
	return doc.RequestID // xAI video generations
}

// maxObjectIDBytes bounds an id the gateway will record.
const maxObjectIDBytes = 512

// recordCreated records the caller as owner of a new object. A failure leaves
// the object unrecorded, which means unreachable: the safe direction.
func (s *Server) recordCreated(ctx context.Context, t *objectTrack, id string, model *store.Model, accessID, requestedModel string) {
	if id == "" || len(id) > maxObjectIDBytes {
		return
	}
	parent := ""
	if t.record != nil {
		parent = t.record.ObjectID
	}
	err := s.Store.RecordUpstreamObject(context.WithoutCancel(ctx), store.UpstreamObject{
		UpstreamID: model.UpstreamID, ObjectID: id, Kind: t.ep.Creates,
		OwnerType: t.ownerType, OwnerID: t.ownerID,
		ModelID: model.ID, AccessID: accessID, RequestedModel: requestedModel, ParentID: parent,
	})
	if err != nil {
		s.Logger.ErrorContext(ctx, "record provider object owner; the object stays unreachable",
			"error", err.Error(), "kind", t.ep.Creates, "upstream_id", model.UpstreamID, "request_id", RequestIDFrom(ctx))
		return
	}
	if endpoints.IsJob(t.ep.Creates) && t.detail != nil {
		// The job runs on at the provider; it is metered when it ends.
		encoded, _ := json.Marshal(t.detail)
		if err := s.Store.OpenObjectJob(context.WithoutCancel(ctx), model.UpstreamID, id, string(encoded)); err != nil {
			s.Logger.ErrorContext(ctx, "open provider job for metering", "error", err.Error(), "kind", t.ep.Creates, "request_id", RequestIDFrom(ctx))
		}
	}
}

// interceptTracked runs between the provider's answer and the relay: it
// records the owner of a created object before the caller can see its id,
// filters a list down to the caller's objects, and notes a delete.
func (s *Server) interceptTracked(ctx context.Context, t *objectTrack, resp *http.Response, model *store.Model, accessID, requestedModel string) {
	if t == nil || resp.StatusCode < 200 || resp.StatusCode > 299 {
		return
	}
	creates := t.ep.Creates != ""
	lists := t.ep.Op == endpoints.OpList
	deletes := t.ep.Op == endpoints.OpDelete && t.record != nil
	if deletes {
		if err := s.Store.MarkUpstreamObjectDeleted(context.WithoutCancel(ctx), t.record.UpstreamID, t.record.ObjectID); err != nil {
			s.Logger.WarnContext(ctx, "mark provider object deleted", "error", err.Error())
		}
	}
	if !creates && !lists {
		return
	}
	if isEventStream(resp.Header) {
		if creates {
			resp.Body = &createdIDSniffer{body: resp.Body, found: func(id string) {
				s.recordCreated(ctx, t, id, model, accessID, requestedModel)
			}}
		}
		return
	}
	limit := s.Config.MaxResponseBytes
	payload, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil || int64(len(payload)) > limit {
		if lists {
			// A list the gateway cannot read cannot be filtered.
			failTracked(resp, "The provider's list was too large for the gateway to filter.")
			return
		}
		resp.Body = struct {
			io.Reader
			io.Closer
		}{io.MultiReader(bytes.NewReader(payload), resp.Body), resp.Body}
		return
	}
	_ = resp.Body.Close()
	if creates {
		s.recordCreated(ctx, t, createdObjectID(t.ep, payload), model, accessID, requestedModel)
	}
	if lists {
		filtered, ferr := s.filterOwnedList(ctx, t, model.UpstreamID, payload)
		if ferr != nil {
			s.Logger.WarnContext(ctx, "filter provider list", "error", ferr.Error(), "family", t.ep.Family)
			failTracked(resp, "The provider's list could not be filtered to your objects.")
			return
		}
		payload = filtered
	}
	resp.Body = io.NopCloser(bytes.NewReader(payload))
	resp.Header.Del("Content-Length")
}

// failTracked turns a provider answer the gateway cannot make safe into an error.
func failTracked(resp *http.Response, message string) {
	body, _ := json.Marshal(map[string]any{"error": map[string]any{
		"message": message, "type": "server_error", "code": CodeUpstreamDown,
	}})
	_ = resp.Body.Close()
	resp.StatusCode = http.StatusBadGateway
	resp.Header = http.Header{"Content-Type": {"application/json"}}
	resp.Body = io.NopCloser(bytes.NewReader(body))
}

// filterOwnedList keeps the entries of a provider list that the caller owns.
// Every other field (object, has_more, paging cursors) is left as it was,
// except first_id and last_id, which describe the entries that remain.
func (s *Server) filterOwnedList(ctx context.Context, t *objectTrack, upstreamID string, payload []byte) ([]byte, error) {
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(payload, &doc); err != nil {
		return nil, err
	}
	raw, ok := doc["data"]
	if !ok {
		return nil, errors.New("the list has no data array")
	}
	var entries []json.RawMessage
	if err := json.Unmarshal(raw, &entries); err != nil {
		return nil, err
	}
	ids := make([]string, len(entries))
	for i, entry := range entries {
		var item struct {
			ID        string `json:"id"`
			RequestID string `json:"request_id"`
		}
		_ = json.Unmarshal(entry, &item)
		ids[i] = item.ID
		if ids[i] == "" {
			ids[i] = item.RequestID
		}
	}
	owned, err := s.Store.OwnedObjectIDs(ctx, upstreamID, t.ownerType, t.ownerID, ids)
	if err != nil {
		return nil, err
	}
	kept := make([]json.RawMessage, 0, len(entries))
	var keptIDs []string
	for i, entry := range entries {
		if ids[i] != "" && owned[ids[i]] {
			kept = append(kept, entry)
			keptIDs = append(keptIDs, ids[i])
		}
	}
	doc["data"], _ = json.Marshal(kept)
	for key, pick := range map[string]int{"first_id": 0, "last_id": len(keptIDs) - 1} {
		if _, present := doc[key]; !present {
			continue
		}
		if len(keptIDs) == 0 {
			doc[key] = json.RawMessage("null")
		} else {
			doc[key], _ = json.Marshal(keptIDs[pick])
		}
	}
	return json.Marshal(doc)
}

// createdIDSniffer reads the id of a streamed create (a Responses stream names
// its response in the first events) and reports it before the bytes that
// carry it are handed on. It looks at the head of the stream only.
type createdIDSniffer struct {
	body  io.ReadCloser
	found func(id string)
	head  []byte
	done  bool
}

// sniffHeadBytes is how much of a stream is searched for the created id.
const sniffHeadBytes = 64 << 10

// streamedResponseID finds the id of the response object a stream event
// carries: usually its first field, otherwise the first resp_ id after it.
var (
	streamedResponseID     = regexp.MustCompile(`"response"\s*:\s*\{\s*"id"\s*:\s*"([^"\\]{1,200})"`)
	streamedResponseIDLate = regexp.MustCompile(`"response"\s*:\s*\{.*?"id"\s*:\s*"(resp_[^"\\]{1,200})"`)
)

func (c *createdIDSniffer) Read(p []byte) (int, error) {
	n, err := c.body.Read(p)
	if n > 0 && !c.done {
		c.head = append(c.head, p[:n]...)
		scanner := bufio.NewScanner(bytes.NewReader(c.head))
		scanner.Buffer(make([]byte, 0, 64<<10), sniffHeadBytes)
		for scanner.Scan() {
			line := scanner.Bytes()
			if !bytes.HasPrefix(line, []byte("data:")) {
				continue
			}
			m := streamedResponseID.FindSubmatch(line)
			if m == nil {
				m = streamedResponseIDLate.FindSubmatch(line)
			}
			if m != nil {
				c.done = true
				c.found(string(m[1]))
				break
			}
		}
		if len(c.head) >= sniffHeadBytes {
			c.done = true
		}
		if c.done {
			c.head = nil
		}
	}
	return n, err
}

func (c *createdIDSniffer) Close() error { return c.body.Close() }

// --- ids in request bodies ---------------------------------------------------

// trackedIDPattern matches the ids providers give stored objects. A request
// that names one in a reference field must own it.
var trackedIDPattern = regexp.MustCompile(`^(file|resp|video|vs|conv|upload|batch|ftjob|asst|thread|run|cntr)[-_][A-Za-z0-9_-]{6,}$`)

// referencePrefixes are how a tracked id starts inside a JSON string.
var referencePrefixes = func() [][]byte {
	var out [][]byte
	for _, family := range []string{"file", "resp", "video", "vs", "conv", "upload", "batch", "ftjob", "asst", "thread", "run", "cntr"} {
		out = append(out, []byte(`"`+family+`-`), []byte(`"`+family+`_`))
	}
	return out
}()

// referenceKeys are the fields that refer to a stored object besides the
// ones named *_id and *_ids.
var referenceKeys = map[string]bool{
	"training_file": true, "validation_file": true, "conversation": true, "container": true,
}

// isReferenceKey reports whether a field's value refers to a stored object.
func isReferenceKey(key string) bool {
	return referenceKeys[key] || strings.HasSuffix(key, "_id") || strings.HasSuffix(key, "_ids")
}

// referencedObjectIDs lists the stored-object ids a JSON request body refers
// to: strings shaped like a provider object id, under a reference field (or
// in a list or an {"id": ...} object directly under one).
func referencedObjectIDs(body []byte) []string {
	// Nearly every request refers to nothing; do not decode those.
	mentions := false
	for _, prefix := range referencePrefixes {
		if bytes.Contains(body, prefix) {
			mentions = true
			break
		}
	}
	if !mentions {
		return nil
	}
	var doc any
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	if dec.Decode(&doc) != nil {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	var walk func(v any, ref bool, depth int)
	walk = func(v any, ref bool, depth int) {
		if depth > 64 {
			return
		}
		switch x := v.(type) {
		case string:
			if ref && len(x) <= maxObjectIDBytes && trackedIDPattern.MatchString(x) && !seen[x] {
				seen[x] = true
				out = append(out, x)
			}
		case []any:
			for _, e := range x {
				walk(e, ref, depth+1)
			}
		case map[string]any:
			for k, e := range x {
				// {"conversation": {"id": "conv_…"}}: the id of an object
				// that is itself the value of a reference field.
				walk(e, isReferenceKey(k) || (ref && k == "id"), depth+1)
			}
		}
	}
	walk(doc, false, 0)
	return out
}

// checkReferences refuses a request that refers to a stored object the caller
// did not create through the gateway.
func (s *Server) checkReferences(ctx context.Context, ownerType, ownerID string, body []byte) *APIError {
	ids := referencedObjectIDs(body)
	if len(ids) == 0 {
		return nil
	}
	owned, err := s.Store.OwnedObjectIDs(ctx, "", ownerType, ownerID, ids)
	if err != nil {
		return ErrInternal()
	}
	for _, id := range ids {
		if !owned[id] {
			return newError(http.StatusNotFound, CodeNotFound, "invalid_request_error",
				"The request refers to "+truncate(id, 80)+", which was not found. Only objects you created through this gateway can be used.")
		}
	}
	return nil
}
