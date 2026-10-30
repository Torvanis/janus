package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"strings"
	"time"

	"github.com/torvanis/janus/internal/alerting"
	"github.com/torvanis/janus/internal/secgw"
	"github.com/torvanis/janus/internal/secgw/classify"
	"github.com/torvanis/janus/internal/secgw/stream"
	"github.com/torvanis/janus/internal/store"
	"github.com/torvanis/janus/internal/usage"
)

// Family safety on the proxy path and its admin endpoint. Design:
// ~/audits/janus-chat/family-filter-design.md.
//
// What the child sees is fixed and kind: no category names, no policy
// names, no error param. What the parent sees is the existing violations
// list and an in-app notification (never the child's words).

// Notices shown to a child covered by Family safety.
const (
	familyBlockedNotice     = "Sorry, that's not something I can help with here. You could try asking in a different way, or talk it over with a grown-up you trust."
	familyReplyStopNotice   = "Sorry, I stopped that answer because it isn't something I can help with here. You could try asking in a different way, or talk it over with a grown-up you trust."
	familyUnavailableNotice = "Sorry, I can't check messages right now, so I can't answer. Please try again in a little while."
	familyEndpointNotice    = "Sorry, that isn't available on this account."
	familyModelNotice       = "Sorry, that model isn't available on this account. Please pick a different one."
	// childNoMediaUpload refuses a sound or video upload from a child whose
	// age range may not turn speech into text.
	childNoMediaUpload = "Sorry, sound and video files aren't available on this account. You can type your question instead."
	// childNoMediaWarning is the Chat note when such a file (attached
	// earlier) is left out of the request.
	childNoMediaWarning = "Sound and video files are not available on this account, so attached ones were not sent."
	// familyWithheldNote ends a reply that was about to give a
	// schoolwork answer away (Child Safety, tutoring).
	familyWithheldNote = "I almost gave you the answer there, but you'll learn more by working it out yourself. Try the next step, then tell me what you get and I'll let you know if you're on the right track."
)

// FamilySupportMessage is the reply a child gets, instead of a model's
// answer, when their message reads as them feeling low or unsafe. It is
// warm, points to a trusted adult first, gives the US crisis lines, and is
// honest that the adult who manages the account may be told they could use
// support (but not what they wrote).
const FamilySupportMessage = "I'm really glad you told me. It sounds like things might feel hard right now, and what you're feeling matters. You don't have to deal with it on your own.\n\n" +
	"Talking to a grown-up you trust can really help: a parent, a teacher, a school counselor, or another adult you feel safe with.\n\n" +
	"If you ever feel like you might hurt yourself, or you just need someone to talk to right now, you can call or text **988** (the Suicide & Crisis Lifeline) any time, day or night. It's free and they're kind. If you're in danger right now, call **911**. If home doesn't feel safe, you can call Childhelp at **1-800-422-4453**.\n\n" +
	"The adult who looks after this account may get a note that you could use some support, but not what you wrote. I'm still here if you want to talk about something else."

// familyNotifyEvery throttles "blocked" notifications per child.
const familyNotifyEvery = 15 * time.Minute

// familyAllowedPaths are the endpoints a covered user may call: the ones
// whose replies Child Safety can judge and whose prompts it can add its
// instructions to (embeddings return vectors, which nobody reads).
// Everything else is refused for covered users: a new endpoint is closed
// to children until it is added here deliberately.
var familyAllowedPaths = map[string]bool{"/v1/chat/completions": true, "/v1/completions": true, "/v1/embeddings": true}

// Media kinds an age range switches on or off (Child Safety → age range →
// What they can make). Music runs on the chat endpoint (Lyria), so it is
// decided by the model, not the path.
const (
	familyMediaImage         = "image"
	familyMediaVideo         = "video"
	familyMediaMusic         = "music"
	familyMediaTranscription = "transcription"
)

// familyMediaPath is the media kind of an endpoint a covered user may call
// when their age range allows it ("" for any other endpoint).
func familyMediaPath(path string) string {
	switch {
	case path == "/v1/images/generations":
		return familyMediaImage
	case path == "/v1/videos" || strings.HasPrefix(path, "/v1/videos/"):
		return familyMediaVideo
	case path == "/v1/audio/transcriptions":
		return familyMediaTranscription
	}
	return ""
}

// familyMediaAllowed reports whether a child's age range switches a media
// kind on. Every kind is off unless an administrator turned it on.
func familyMediaAllowed(p *store.ChildProfile, kind string) bool {
	if p == nil {
		return true
	}
	switch kind {
	case familyMediaImage:
		return p.Media.Image
	case familyMediaVideo:
		return p.Media.Video
	case familyMediaMusic:
		return p.Media.Music
	case familyMediaTranscription:
		return p.Media.Transcription
	}
	return false
}

// childProfileFor is the age range that applies to a user in Chat, or nil
// when Child Safety does not cover them.
func (s *Server) childProfileFor(ctx context.Context, user *store.User) *store.ChildProfile {
	if user == nil {
		return nil
	}
	eng, err := s.secgwEngine(ctx)
	if err != nil {
		return nil
	}
	return eng.FamilyProfile(user.ID)
}

// childMediaOff reports whether Chat must not offer a media kind to this
// user (a covered child whose age range has it switched off).
func (s *Server) childMediaOff(ctx context.Context, user *store.User, kind string) bool {
	p := s.childProfileFor(ctx, user)
	return p != nil && !familyMediaAllowed(p, kind)
}

// childMediaOffList is the media kinds a child's age range switches off
// (empty when Child Safety does not cover them).
func childMediaOffList(p *store.ChildProfile) []string {
	out := []string{}
	if p == nil {
		return out
	}
	for _, k := range []string{familyMediaImage, familyMediaVideo, familyMediaMusic, familyMediaTranscription} {
		if !familyMediaAllowed(p, k) {
			out = append(out, k)
		}
	}
	return out
}

// familyMediaPrompt is the text a media request asks for: the JSON
// "prompt" (images, videos) or the multipart "prompt" field
// (transcription). Uploaded files are not text and are not judged.
func familyMediaPrompt(r *http.Request, body []byte) string {
	ct := r.Header.Get("Content-Type")
	if strings.HasPrefix(ct, "multipart/") {
		_, params, err := mime.ParseMediaType(ct)
		if err != nil || params["boundary"] == "" {
			return ""
		}
		mr := multipart.NewReader(bytes.NewReader(body), params["boundary"])
		var parts []string
		for {
			part, err := mr.NextPart()
			if err != nil {
				break
			}
			if part.FormName() == "prompt" && part.FileName() == "" {
				b, _ := io.ReadAll(io.LimitReader(part, 64<<10))
				if t := strings.TrimSpace(string(b)); t != "" {
					parts = append(parts, t)
				}
			}
			_ = part.Close()
		}
		return strings.Join(parts, "\n\n")
	}
	var in struct {
		Prompt string `json:"prompt"`
	}
	if json.Unmarshal(body, &in) != nil {
		return ""
	}
	return strings.TrimSpace(in.Prompt)
}

// familyModelBlocked reports whether a covered user's age range blocks the
// model a request names or is served by.
func familyModelBlocked(eng *secgw.Engine, userID string, names ...string) bool {
	p := eng.FamilyProfile(userID)
	return p != nil && p.Blocks(names...)
}

// childFilterCatalog hides the models a covered child's age range blocks
// from a model listing (catalog, managed and personal-plan models), so the
// picker never offers what the proxy would refuse.
func (s *Server) childFilterCatalog(ctx context.Context, user *store.User, catalog *GrantedCatalog, personal *[]map[string]any) {
	if user == nil {
		return
	}
	eng, err := s.secgwEngine(ctx)
	if err != nil {
		return
	}
	p := eng.FamilyProfile(user.ID)
	if p == nil || len(p.BlockedModels) == 0 {
		return
	}
	models := catalog.Models[:0:0]
	for _, m := range catalog.Models {
		if !p.Blocks(m.ID, m.PublicName(), m.Name) {
			models = append(models, m)
		}
	}
	catalog.Models = models
	managed := catalog.Managed[:0:0]
	for _, m := range catalog.Managed {
		if !p.Blocks(m.ID, m.Name) {
			managed = append(managed, m)
		}
	}
	catalog.Managed = managed
	if personal != nil {
		out := (*personal)[:0:0]
		for _, m := range *personal {
			id, _ := m["id"].(string)
			if !p.Blocks(id) {
				out = append(out, m)
			}
		}
		*personal = out
	}
}

// injectChildInstructions adds Child Safety's instructions to a request:
// the system prompt of a chat body, or ahead of a legacy completions
// prompt. It reports false when it could not (the request is refused).
func injectChildInstructions(path string, body []byte, text string) ([]byte, bool) {
	if strings.TrimSpace(text) == "" {
		return body, true
	}
	switch path {
	case "/v1/chat/completions":
		return injectInstructions(path, body, text)
	case "/v1/completions":
		var m map[string]json.RawMessage
		if json.Unmarshal(body, &m) != nil {
			return body, false
		}
		var prompt string
		if json.Unmarshal(m["prompt"], &prompt) != nil {
			return body, false
		}
		raw, _ := json.Marshal(text + "\n\n" + prompt)
		m["prompt"] = raw
		out, err := json.Marshal(m)
		return out, err == nil
	case "/v1/embeddings":
		return body, true
	}
	if familyMediaPath(path) != "" {
		// A media request has no system prompt to add to; its prompt was
		// judged on the way in.
		return body, true
	}
	return body, false
}

// errFamily is the refusal a covered child gets: the security-blocked code
// with a kind message and no param (the check kind is an internal detail).
func errFamily(msg string) *APIError {
	return newError(http.StatusForbidden, CodeSecurityBlocked, "permission_error", msg)
}

// familyNotice is what to tell the parent once the event is recorded.
type familyNotice struct {
	userID string
	action string // blocked | supported
	reply  bool
	failed bool
}

// attachFamilyNotice chains the parent notification after whatever the
// event's AfterInsert already does (recording violations), so it runs on
// the tracked usage-writer goroutine once the rows exist.
func (s *Server) attachFamilyNotice(event *store.UsageEvent, n *familyNotice) {
	if n == nil || s.Alerts == nil || n.failed {
		return
	}
	prev := event.AfterInsert
	event.AfterInsert = func(ctx context.Context) {
		if prev != nil {
			prev(ctx)
		}
		s.notifyFamily(ctx, n)
	}
}

func (s *Server) notifyFamily(ctx context.Context, n *familyNotice) {
	if n.action != store.SecgwActionSupported {
		// The throttle is claimed in the database: a child's requests are
		// spread across replicas, and the guardian gets one notice per
		// window from the whole cluster.
		if !s.claimAlert(ctx, "family_blocked:"+n.userID, familyNotifyEvery) {
			return
		}
	}
	name := "A child"
	if u, err := s.Store.UserByID(ctx, n.userID); err == nil {
		if u.Name != "" {
			name = u.Name
		} else if u.Email != "" {
			name = u.Email
		}
	}
	ev := alerting.Event{Trigger: alerting.TriggerFamilySafety, NotifyAdmins: true,
		Data: map[string]any{"user_id": n.userID, "action": n.action, "policy": store.SecgwFamilyPolicyID}}
	switch {
	case n.action == store.SecgwActionSupported:
		ev.Severity = "critical"
		ev.Title = "Child Safety: " + name + " may need support"
		ev.Body = "Something " + name + " wrote suggests they may be feeling low or unsafe. Janus answered with a kind message pointing them to a trusted adult and to the 988 Suicide & Crisis Lifeline, and did not pass the message to the model. Child Safety does not keep their words (Chat keeps the conversation as usual). Consider checking in with them."
	case n.reply:
		ev.Severity = "warning"
		ev.Title = "Child Safety stopped a reply to " + name
		ev.Body = "A model's reply to " + name + " was stopped because it was not suitable. " + name + " saw a short, kind notice. Details are under Security → Violations."
	default:
		ev.Severity = "warning"
		ev.Title = "Child Safety blocked a message from " + name
		ev.Body = name + " asked something Child Safety does not allow and saw a short, kind notice instead of an answer. Details are under Security → Violations."
	}
	s.Alerts.Dispatch(ctx, ev)
}

// familyIngress runs Family safety on a covered user's request. It returns
// true when it answered the request itself (blocked or supported).
func (s *Server) familyIngress(w http.ResponseWriter, r *http.Request, eng *secgw.Engine, event *store.UsageEvent, body []byte, prior []secgw.Violation, policy *store.SecgwPolicy, started time.Time, instr *string) bool {
	profile := eng.FamilyProfile(event.UserID)
	kind := familyMediaPath(r.URL.Path)
	if kind == "" && (chatMusicModelName(event.ModelName) || chatMusicModelName(event.RequestedModelName)) {
		kind = familyMediaMusic
	}
	if (!familyAllowedPaths[r.URL.Path] && kind == "") || (kind != "" && !familyMediaAllowed(profile, kind)) {
		event.SecgwAction = store.SecgwActionBlocked
		s.attachSecgwViolations(event, policy, prior)
		s.rejectProxyEvent(w, r, errFamily(familyEndpointNotice), event, started)
		return true
	}
	judged := body
	if familyMediaPath(r.URL.Path) != "" {
		// A media request is judged on its prompt only (a status poll or
		// a download has none and needs no check).
		prompt := familyMediaPrompt(r, body)
		if prompt == "" {
			return false
		}
		judged, _ = json.Marshal(map[string]string{"prompt": prompt})
	}
	out := eng.FamilyIngress(r.Context(), event.UserID, judged)
	event.ClassifierMs += int(out.Model.Elapsed.Milliseconds())
	event.ClassifierCalls += out.Model.Calls
	if out.Err != nil {
		s.Logger.WarnContext(r.Context(), "secgw: family safety classifier failed, failing closed", "error", out.Err.Error(), "request_id", event.RequestID)
	}
	if out.Action == secgw.FamilyAllow {
		if r.URL.Path != "/v1/embeddings" {
			*instr = out.Instructions
			if len(out.Matched) > 0 {
				event.RouteReason = withChildNote(event.RouteReason, strings.Join(out.Matched, ","))
			}
		}
		return false
	}
	vs := prior
	if out.Violation != nil {
		vs = append(append([]secgw.Violation{}, prior...), *out.Violation)
	}
	event.SecgwViolations = secgw.Recorded(vs)
	s.attachSecgwViolations(event, policy, vs)
	n := &familyNotice{userID: event.UserID, action: out.Action, failed: out.Failed}
	if out.ToolOutput {
		// Tool output the client sent (a page, search results) was not
		// suitable: refused, but it is not the child's words, so no
		// notice tells a parent the child asked for it.
		n = nil
	}
	if out.Action == secgw.FamilySupport {
		event.SecgwAction = store.SecgwActionSupported
		s.attachFamilyNotice(event, n)
		s.writeFamilySupport(w, r, event, started)
		return true
	}
	event.SecgwAction = store.SecgwActionBlocked
	s.attachFamilyNotice(event, n)
	msg := familyBlockedNotice
	if out.Failed {
		msg = familyUnavailableNotice
	}
	s.rejectProxyEvent(w, r, errFamily(msg), event, started)
	return true
}

// writeFamilySupport answers with FamilySupportMessage as a normal, well
// formed completion (stream or not), so every client shows it as the
// assistant's reply. Nothing was forwarded, so nothing is billed.
func (s *Server) writeFamilySupport(w http.ResponseWriter, r *http.Request, event *store.UsageEvent, started time.Time) {
	event.HTTPStatus = http.StatusOK
	event.LatencyMs = int(time.Since(started).Milliseconds())
	event.AccountingMode = usage.AccountingNone
	event.FinishReason = "stop"
	w.Header().Set(headerSecgwAction, store.SecgwActionSupported)
	id := "chatcmpl-" + event.RequestID
	created := time.Now().Unix()
	legacy := r.URL.Path == "/v1/completions"
	if event.Streaming {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		choice := map[string]any{"index": 0, "delta": map[string]any{"role": "assistant", "content": FamilySupportMessage}, "finish_reason": "stop"}
		object := "chat.completion.chunk"
		if legacy {
			choice, object = map[string]any{"index": 0, "text": FamilySupportMessage, "finish_reason": "stop"}, "text_completion"
		}
		payload, _ := json.Marshal(map[string]any{"id": id, "object": object, "created": created, "model": event.ModelName, "choices": []any{choice}})
		_, _ = w.Write(append(append([]byte("data: "), payload...), '\n', '\n'))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	} else {
		choice := map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": FamilySupportMessage}, "finish_reason": "stop"}
		object := "chat.completion"
		if legacy {
			choice, object = map[string]any{"index": 0, "text": FamilySupportMessage, "finish_reason": "stop"}, "text_completion"
		}
		WriteJSON(w, http.StatusOK, map[string]any{"id": id, "object": object, "created": created, "model": event.ModelName,
			"choices": []any{choice}, "usage": map[string]any{"prompt_tokens": 0, "completion_tokens": 0, "total_tokens": 0}})
	}
	s.Metrics.ObserveGateway("/v1/*", event.Modality, "", r.Method, http.StatusOK, time.Since(started))
	s.writeUsage(r.Context(), event, "write family support usage event", func(bg context.Context) {
		event.RunAfterInsert(bg)
		s.finishCapture(bg, event)
	})
}

// keepChildNote is next with any Child Safety note carried over from prev.
func keepChildNote(prev, next string) string {
	if i := strings.Index(prev, "child="); i >= 0 {
		note := prev[i:]
		if j := strings.Index(note, ";"); j >= 0 {
			note = note[:j]
		}
		if next == "" {
			return note
		}
		return next + ";" + note
	}
	return next
}

// withChildNote records on the route reason which Child Safety categories
// were answered appropriately ("...;child=homework_learning").
func withChildNote(reason, slugs string) string {
	note := "child=" + slugs
	if reason == "" {
		return note
	}
	return reason + ";" + note
}

// familyEgress carries what the reply side of Family safety needs.
type familyEgress struct {
	eng    *secgw.Engine
	prompt string
	sink   *[]secgw.Violation
	notice *familyNotice
	// withheld: the reply stopped where it would have given a schoolwork
	// answer away; the stream ends with familyWithheldNote.
	withheld bool
}

// newFamilyEgress returns the reply-side state for a covered user, or nil.
func newFamilyEgress(eng *secgw.Engine, event *store.UsageEvent, path string, body []byte, sink *[]secgw.Violation) *familyEgress {
	if eng == nil || !eng.FamilyCovers(event.UserID) || path == "/v1/embeddings" {
		return nil
	}
	return &familyEgress{eng: eng, prompt: secgw.FamilyPrompt(body), sink: sink}
}

// judge decides one reply chunk, recording what it cost and found.
func (f *familyEgress) judge(ctx context.Context, s *Server, event *store.UsageEvent, prior, chunk string) bool {
	out := f.eng.FamilyJudgeReply(ctx, event.UserID, f.prompt, prior, chunk)
	event.ClassifierMs += int(out.Model.Elapsed.Milliseconds())
	event.ClassifierCalls += out.Model.Calls
	if out.Err != nil {
		s.Logger.WarnContext(ctx, "secgw: family safety reply check failed, failing closed", "error", out.Err.Error(), "request_id", event.RequestID)
	}
	if out.Action == secgw.FamilyAllow {
		return false
	}
	if out.Action == secgw.FamilyWithhold {
		f.withheld = true
	}
	if out.Violation != nil {
		v := *out.Violation
		v.Action = store.SecgwActionStreamCut
		if !event.Streaming {
			v.Action = store.SecgwActionBlocked
		}
		if f.withheld {
			v.Action = store.SecgwActionWithheld
		}
		*f.sink = append(*f.sink, v)
	}
	if !f.withheld {
		// A withheld answer is the tutor working, not an incident.
		f.notice = &familyNotice{userID: event.UserID, action: store.SecgwActionBlocked, reply: true, failed: out.Failed}
	}
	return true
}

// gate builds the streaming reply gate.
func (f *familyEgress) gate(ctx context.Context, s *Server, event *store.UsageEvent) *stream.ParagraphGate {
	return stream.NewParagraphGate(func(_, prior, chunk string) (bool, any) {
		return f.judge(ctx, s, event, prior, chunk), nil
	})
}

// writeFamilyStreamCut ends a stream Family safety stopped: the existing
// in-band security error frame with the kind notice and no param.
func (s *Server) writeFamilyStreamCut(w http.ResponseWriter, flusher http.Flusher, event *store.UsageEvent, failed, withheld bool) {
	if withheld {
		// The tutor's note ends the reply as ordinary text: the child sees
		// what was cleared, then the note, and the reply finishes normally.
		event.SecgwAction = store.SecgwActionWithheld
		event.FinishReason = "stop"
		frame := func(delta map[string]any, finish any) {
			payload, _ := json.Marshal(map[string]any{"id": "chatcmpl-" + event.RequestID, "object": "chat.completion.chunk", "created": time.Now().Unix(),
				"model": event.ModelName, "choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}}})
			_, _ = w.Write(append(append([]byte("data: "), payload...), '\n', '\n'))
		}
		frame(map[string]any{"content": "\n\n" + familyWithheldNote}, nil)
		frame(map[string]any{}, "stop")
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
		if flusher != nil {
			flusher.Flush()
		}
		return
	}
	event.SecgwAction = store.SecgwActionStreamCut
	event.ErrorCode = CodeSecurityBlocked
	s.Metrics.PolicyRejections.WithLabelValues(CodeSecurityBlocked).Inc()
	msg := familyReplyStopNotice
	if failed {
		msg = familyUnavailableNotice
	}
	apiErr := errFamily(msg)
	apiErr.RequestID = event.RequestID
	payload, err := json.Marshal(map[string]any{"error": apiErr})
	if err != nil {
		return
	}
	if _, err := w.Write(append(append([]byte("data: "), payload...), '\n', '\n')); err != nil {
		return
	}
	_, _ = w.Write([]byte("data: [DONE]\n\n"))
	if flusher != nil {
		flusher.Flush()
	}
}

// --- coverage loss -------------------------------------------------------------

// childCoveredNow is who Child Safety covers before a change to teams,
// for childCoverageLost afterwards (nil when it cannot be read: no warning).
func (s *Server) childCoveredNow(ctx context.Context) map[string]bool {
	before, err := s.Store.ChildCovered(ctx)
	if err != nil {
		return nil
	}
	return before
}

// childCoverageLost tells administrators (and the Child Safety
// notification channel) about every person a team change took out of
// Child Safety: a team archived, a member removed, a directory or SCIM
// sync. how says what happened, in plain words.
func (s *Server) childCoverageLost(ctx context.Context, before map[string]bool, how string) {
	if len(before) == 0 {
		return
	}
	after, err := s.Store.ChildCovered(ctx)
	if err != nil {
		return
	}
	var lost []string
	for uid := range before {
		if !after[uid] {
			lost = append(lost, uid)
		}
	}
	if len(lost) == 0 {
		return
	}
	s.InvalidateConfigCache()
	if s.Alerts == nil {
		return
	}
	for _, uid := range lost {
		name := "A child"
		if u, err := s.Store.UserByID(ctx, uid); err == nil {
			if u.Name != "" {
				name = u.Name
			} else if u.Email != "" {
				name = u.Email
			}
		}
		s.Alerts.Dispatch(ctx, alerting.Event{Trigger: alerting.TriggerFamilySafety, NotifyAdmins: true, Severity: "warning",
			Title: "Child Safety no longer covers " + name,
			Body: name + " is no longer under Child Safety because " + how + ". Their requests now get no Child Safety checks. " +
				"To protect them again, assign them (or a team they are in) an age range under Administration → Security → Child Safety.",
			Data: map[string]any{"user_id": uid, "action": "coverage_lost", "policy": store.SecgwFamilyPolicyID}})
	}
}

// --- admin ---------------------------------------------------------------------

// familyClassifier is one model the family policy can use.
type familyClassifier struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Role        string `json:"classifier_role"`
	UpstreamID  string `json:"upstream_id"`
	Upstream    string `json:"upstream_name"`
	Hosted      bool   `json:"hosted"`
	Recommended string `json:"recommended,omitempty"`
}

// familyRecommendation names what a model is good for in the picker.
func familyRecommendation(name string, hosted bool) string {
	n := strings.ToLower(name)
	switch {
	case strings.Contains(n, "jev"):
		return "hosted_fastest"
	case strings.Contains(n, "clef-flash") || strings.Contains(n, "clef_flash") || strings.Contains(n, "nimble"):
		return "local_fast"
	case strings.Contains(n, "clef"):
		return "local"
	case strings.Contains(n, "guard"):
		return "legacy"
	}
	if hosted {
		return "hosted"
	}
	return ""
}

func hostedURL(base string) bool {
	b := strings.ToLower(base)
	return strings.Contains(b, "openrouter.ai") || strings.Contains(b, "typesafe.ai") || strings.Contains(b, "api.openai.com") || strings.Contains(b, ".openai.azure.com")
}

func (s *Server) familyClassifiers(ctx context.Context) ([]familyClassifier, error) {
	models, err := s.Store.ListModels(ctx, store.ModelFilter{ClassifierOnly: true})
	if err != nil {
		return nil, err
	}
	out := []familyClassifier{}
	ups := map[string]*store.Upstream{}
	for _, m := range models {
		if !store.ClassifierRoleServes(m.ClassifierRole, store.SecgwCheckContentSafety) {
			continue
		}
		up := ups[m.UpstreamID]
		if up == nil {
			if up, err = s.Store.UpstreamByID(ctx, m.UpstreamID); err != nil {
				continue
			}
			ups[m.UpstreamID] = up
		}
		hosted := hostedURL(up.BaseURL)
		out = append(out, familyClassifier{ID: m.ID, Name: m.PublicName(), Role: m.ClassifierRole, UpstreamID: up.ID, Upstream: up.Name,
			Hosted: hosted, Recommended: familyRecommendation(m.Name, hosted)})
	}
	return out, nil
}

// familyStatus is the card's one verdict.
type familyStatus struct {
	// State is protecting, not_protecting or off.
	State   string   `json:"state"`
	Reasons []string `json:"reasons"`
	Covered int      `json:"covered_users"`
}

func (s *Server) familyStatusFor(ctx context.Context, cfg *store.SecgwFamilyConfig, classifiers []familyClassifier) familyStatus {
	st := familyStatus{State: "off", Reasons: []string{}}
	if !cfg.Enabled {
		return st
	}
	if covered, err := s.Store.LoadSecgwFamilySnapshot(ctx); err == nil {
		st.Covered = len(covered.Covered)
	}
	switch {
	case cfg.ClassifierModelID == "":
		st.Reasons = append(st.Reasons, "no_model")
	default:
		found := false
		for _, c := range classifiers {
			if c.ID == cfg.ClassifierModelID {
				found = true
			}
		}
		if !found {
			st.Reasons = append(st.Reasons, "model_unavailable")
		} else if cl, err := s.secgwClassifiers().For(ctx, cfg.ClassifierModelID); err != nil {
			st.Reasons = append(st.Reasons, "model_unavailable")
		} else if b, ok := cl.(*classify.Breaker); ok && b.Open() {
			st.Reasons = append(st.Reasons, "model_down")
		}
	}
	if len(cfg.Assignments) == 0 {
		st.Reasons = append(st.Reasons, "no_one_assigned")
	}
	if len(st.Reasons) == 0 {
		st.State = "protecting"
	} else {
		st.State = "not_protecting"
	}
	return st
}

type familyCategoryView struct {
	Slug     string `json:"slug"`
	Name     string `json:"name"`
	Includes string `json:"includes"`
	Excludes string `json:"excludes,omitempty"`
}

func childCategoryView(slug string) familyCategoryView {
	c, _ := store.ChildCategory(slug)
	if c == nil {
		return familyCategoryView{Slug: slug, Name: slug}
	}
	return familyCategoryView{Slug: slug, Name: c.Name, Includes: c.Includes, Excludes: c.Excludes}
}

func (s *Server) handleGetSecgwFamily(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	cfg, err := s.Store.SecgwFamily(ctx)
	if err != nil {
		writeSecgwError(w, r, err)
		return
	}
	classifiers, err := s.familyClassifiers(ctx)
	if err != nil {
		writeSecgwError(w, r, err)
		return
	}
	cats := []familyCategoryView{}
	for _, slug := range store.ChildSafetyCategorySlugs {
		cats = append(cats, childCategoryView(slug))
	}
	safety := []familyCategoryView{}
	for _, slug := range store.SecgwFamilySafetySlugs {
		safety = append(safety, childCategoryView(slug))
	}
	builtins := map[string]store.ChildBuiltinText{}
	for _, p := range cfg.Profiles {
		builtins[p.ID] = store.ChildBuiltin(p.ID)
	}
	recent, err := s.Store.ListSecgwViolations(ctx, store.SecgwViolationFilter{PolicyID: store.SecgwFamilyPolicyID, Limit: 20})
	if err != nil {
		writeSecgwError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{
		"family": cfg, "categories": cats, "safety_categories": safety, "classifiers": classifiers,
		"builtin": builtins, "tutor_rule": store.TutorRule(),
		"status": s.familyStatusFor(ctx, cfg, classifiers), "recent": recent, "support_message": FamilySupportMessage,
		"withheld_note": familyWithheldNote,
	})
}

func (s *Server) handlePutSecgwFamily(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var in store.SecgwFamilyInput
	if err := decodeJSON(r, &in); err != nil {
		writeSecgwError(w, r, err)
		return
	}
	before, err := s.Store.SecgwFamily(ctx)
	if err != nil {
		writeSecgwError(w, r, err)
		return
	}
	in.ClassifierModelID = strings.TrimSpace(in.ClassifierModelID)
	if in.ClassifierModelID != "" {
		m, err := s.Store.ModelByID(ctx, in.ClassifierModelID)
		if errors.Is(err, store.ErrNotFound) {
			WriteError(w, r, ErrInvalidRequest("That safety model does not exist.").WithParam("classifier_model_id"))
			return
		}
		if err != nil {
			writeSecgwError(w, r, err)
			return
		}
		if !store.ClassifierRoleServes(m.ClassifierRole, store.SecgwCheckContentSafety) {
			WriteError(w, r, ErrInvalidRequest(m.PublicName()+" cannot check content safety. Choose a decision model (Jev, Clef, Clef-Flash, Nimble) or a Llama Guard model, and set its classifier role on the Classifiers tab first.").WithParam("classifier_model_id"))
			return
		}
	}
	by := ""
	if u := UserFrom(ctx); u != nil {
		by = u.ID
	}
	if err := s.Store.SetSecgwFamily(ctx, in, by); err != nil {
		writeSecgwError(w, r, err)
		return
	}
	s.InvalidateConfigCache()
	after, _ := s.Store.SecgwFamily(ctx)
	s.audit(r, "secgw_family_updated", "secgw_policy", store.SecgwFamilyPolicyID, familyAudit(before), familyAudit(after))
	s.handleGetSecgwFamily(w, r)
}

func familyAudit(c *store.SecgwFamilyConfig) map[string]any {
	if c == nil {
		return nil
	}
	assigned := make([]string, 0, len(c.Assignments))
	for _, a := range c.Assignments {
		assigned = append(assigned, a.ScopeType+":"+a.ScopeID)
	}
	profiles := make([]string, 0, len(c.Profiles))
	for _, p := range c.Profiles {
		profiles = append(profiles, p.ID)
	}
	for i, a := range c.Assignments {
		assigned[i] += "=" + a.ProfileID
	}
	return map[string]any{"enabled": c.Enabled, "classifier_model_id": c.ClassifierModelID, "profiles": profiles, "ask": c.Ask, "assignments": assigned}
}
