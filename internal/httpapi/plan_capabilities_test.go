package httpapi

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/torvanis/janus/internal/adapter"
	"github.com/torvanis/janus/internal/capability"
	"github.com/torvanis/janus/internal/store"
)

type planCapsBody struct {
	Provider    string               `json:"provider"`
	MaxRequests int                  `json:"max_requests"`
	Models      []planCapabilityView `json:"models"`
}

func planModelView(t *testing.T, body planCapsBody, model string) planCapabilityView {
	t.Helper()
	for _, m := range body.Models {
		if m.Model == model {
			return m
		}
	}
	t.Fatalf("no %s in %+v", model, body.Models)
	return planCapabilityView{}
}

// A plan model's record: built-in default < vendor list < Measure < the
// owner's own setting, readable and settable only by the owner.
func TestPlanCapabilitiesSetAndSources(t *testing.T) {
	h := newHarness(t)
	fake := newFakeXAI(t)
	fake.models = []string{"grok-4.3", "grok-test", "grok-imagine-video"}
	sess := adminSession(t, h)
	h.enableXAISubscriptions(t, sess, true)
	connID, out := h.connectXAI(t, sess)
	if connID == "" {
		t.Fatalf("connect: %v", out)
	}
	path := "/api/v1/me/subscriptions/" + connID + "/capabilities"

	rec := h.doAsSession(sess, http.MethodGet, path, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("get: %d %s", rec.Code, rec.Body.String())
	}
	var body planCapsBody
	decodeInto(t, rec, &body)
	if body.Provider != "xai" || body.MaxRequests != capability.MaxProbeRequests || len(body.Models) != 3 {
		t.Fatalf("body: %+v", body)
	}
	grok := planModelView(t, body, "grok-4.3")
	if r := grok.Resolved[adapter.CapInImage]; r.Value != true || r.Source != capability.SourceDefault || r.Note == "" {
		t.Fatalf("grok-4.3 images should be assumed for xAI plans: %#v", r)
	}
	if r, said := planModelView(t, body, "grok-test").Resolved[adapter.CapInImage]; said {
		t.Fatalf("an undocumented model stays unknown: %#v", r)
	}
	if v := planModelView(t, body, "grok-imagine-video"); v.NotMeasurable == "" {
		t.Fatalf("a video model is not measured: %+v", v)
	}

	// Set by the owner, then back to automatic.
	rec = h.doAsSession(sess, http.MethodPatch, path, map[string]any{"model": "grok-4.3", "set": map[string]any{adapter.CapInImage: false}})
	if rec.Code != http.StatusOK {
		t.Fatalf("set: %d %s", rec.Code, rec.Body.String())
	}
	var view planCapabilityView
	decodeInto(t, rec, &view)
	if r := view.Resolved[adapter.CapInImage]; r.Value != false || r.Source != capability.SourceAdmin {
		t.Fatalf("after set: %#v", r)
	}
	rec = h.doAsSession(sess, http.MethodPatch, path, map[string]any{"model": "grok-4.3", "set": map[string]any{adapter.CapInImage: nil}})
	decodeInto(t, rec, &view)
	if r := view.Resolved[adapter.CapInImage]; rec.Code != http.StatusOK || r.Value != true || r.Source != capability.SourceDefault {
		t.Fatalf("use default: %d %#v", rec.Code, r)
	}
	for _, bad := range []map[string]any{
		{"model": "grok-nope", "set": map[string]any{adapter.CapInImage: true}},
		{"model": "grok-4.3", "set": map[string]any{"input.smell": true}},
		{"model": "grok-4.3", "set": map[string]any{adapter.CapInImage: "yes"}},
		{"model": "grok-4.3"},
	} {
		if rec := h.doAsSession(sess, http.MethodPatch, path, bad); rec.Code != http.StatusBadRequest {
			t.Fatalf("%v: want 400, got %d %s", bad, rec.Code, rec.Body.String())
		}
	}

	// Another person cannot read, set or measure this connection.
	other := h.sessionFor(t, "someone-else@example.com")
	for _, c := range []struct {
		method, path string
		body         any
	}{
		{http.MethodGet, path, nil},
		{http.MethodPatch, path, map[string]any{"model": "grok-4.3", "set": map[string]any{adapter.CapInImage: false}}},
		{http.MethodPost, path + "/measure", map[string]any{"model": "grok-4.3"}},
	} {
		if rec := h.doAsSession(other, c.method, c.path, c.body); rec.Code != http.StatusNotFound {
			t.Fatalf("%s %s by another user: %d %s", c.method, c.path, rec.Code, rec.Body.String())
		}
	}
	conn, err := h.store.SubscriptionByID(context.Background(), connID)
	if err != nil || conn.Capabilities["grok-4.3"].Admin != nil {
		t.Fatalf("the other user's PATCH changed nothing: %v %#v", err, conn.Capabilities)
	}

	// /api/v1/models hands Chat the same answer, with its source.
	h.selectModels(t, sess, connID, "grok-4.3")
	rec = h.doAsSession(sess, http.MethodGet, "/api/v1/models", nil)
	if !strings.Contains(rec.Body.String(), `"input.image":{"value":true,"source":"default"`) {
		t.Fatalf("personal model capabilities in the catalog: %s", rec.Body.String())
	}

	// Turning the provider off closes the routes.
	h.enableXAISubscriptions(t, sess, false)
	if rec := h.doAsSession(sess, http.MethodGet, path, nil); rec.Code != http.StatusForbidden {
		t.Fatalf("provider off: %d %s", rec.Code, rec.Body.String())
	}
}

// Measure runs the probe through the owner's own connection, records only
// what the replies show, never re-measures what the owner set, is limited
// to once per model per window, and is metered on the plan at no cost to
// the organization.
func TestPlanCapabilitiesMeasure(t *testing.T) {
	h := newHarness(t)
	fake := newFakeXAI(t)
	fake.models = []string{"grok-test"}
	fake.answer = func(body string) string {
		switch {
		case strings.Contains(body, `"image_url"`):
			return "Green"
		case strings.Contains(body, `"input_audio"`):
			return "I cannot hear anything."
		case strings.Contains(body, `"video_url"`):
			return "Blue"
		}
		return "43"
	}
	sess := adminSession(t, h)
	h.enableXAISubscriptions(t, sess, true)
	connID, out := h.connectXAI(t, sess)
	if connID == "" {
		t.Fatalf("connect: %v", out)
	}
	path := "/api/v1/me/subscriptions/" + connID + "/capabilities"
	// The owner says video is no: Measure leaves that alone.
	if rec := h.doAsSession(sess, http.MethodPatch, path, map[string]any{"model": "grok-test", "set": map[string]any{adapter.CapInVideo: false}}); rec.Code != http.StatusOK {
		t.Fatalf("set: %d %s", rec.Code, rec.Body.String())
	}
	fake.mu.Lock()
	before := len(fake.inferenceBody)
	fake.mu.Unlock()

	rec := h.doAsSession(sess, http.MethodPost, path+"/measure", map[string]any{"model": "grok-test"})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("measure: %d %s", rec.Code, rec.Body.String())
	}
	var view planCapabilityView
	decodeInto(t, rec, &view)
	if view.Probed == nil || view.Probed.Status != capability.ProbeRunning {
		t.Fatalf("measure answer: %+v", view)
	}
	// Pressing again while it runs, or within the window, is refused.
	if rec := h.doAsSession(sess, http.MethodPost, path+"/measure", map[string]any{"model": "grok-test"}); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("second measure: %d %s", rec.Code, rec.Body.String())
	}
	ctx := context.Background()
	var conn *store.SubscriptionConnection
	for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		c, err := h.store.SubscriptionByID(ctx, connID)
		if err != nil {
			t.Fatal(err)
		}
		if p := c.Capabilities["grok-test"].Probed; p != nil && p.Status != capability.ProbeRunning {
			conn = c
			break
		}
	}
	if conn == nil {
		t.Fatal("measure did not finish")
	}
	st := conn.Capabilities["grok-test"]
	if st.Probed.Status != capability.ProbeOK || st.Probed.Requests == 0 || st.Probed.Requests > capability.MaxProbeRequests {
		t.Fatalf("run: %+v", st.Probed)
	}
	if st.Probe[adapter.CapInImage].Value != true {
		t.Fatalf("named the test image's color, so images are measured yes: %#v", st.Probe)
	}
	if _, said := st.Probe[adapter.CapInAudio]; said {
		t.Fatalf("a reply that hears nothing is no evidence either way: %#v", st.Probe)
	}
	if _, said := st.Probe[adapter.CapInVideo]; said || st.Admin[adapter.CapInVideo].Value != false {
		t.Fatalf("the owner's setting is not measured over: %#v", st)
	}
	fake.mu.Lock()
	sent := fake.inferenceBody[before:]
	auths := fake.inferenceAuth[before:]
	fake.mu.Unlock()
	if len(sent) != st.Probed.Requests {
		t.Fatalf("plan saw %d requests, run says %d", len(sent), st.Probed.Requests)
	}
	for i, b := range sent {
		if strings.Contains(b, `"video_url"`) {
			t.Fatalf("video was measured although the owner set it: %s", b)
		}
		if auths[i] != fake.liveAccess {
			t.Fatalf("request %d did not use the owner's own plan sign-in", i)
		}
	}
	// Recorded on the owner's plan, as Janus's own call, at no cost.
	evs, _, err := h.store.ListRequests(ctx, store.RequestFilter{Limit: 200, IncludeInternal: true})
	if err != nil {
		t.Fatal(err)
	}
	probes := 0
	for _, e := range evs {
		if e.ClientApp == store.InternalProbeClientApp && e.SubscriptionID == connID {
			probes++
			if e.CostNano != 0 || e.UserID != h.user.ID || e.ModelName != "my/xai/grok-test" {
				t.Fatalf("probe event: %+v", e)
			}
		}
	}
	if probes == 0 {
		t.Fatal("no usage recorded for the measurement")
	}
	if rec := h.doAsSession(sess, http.MethodPost, path+"/measure", map[string]any{"model": "grok-test"}); rec.Code != http.StatusTooManyRequests ||
		rec.Header().Get("Retry-After") == "" || !strings.Contains(rec.Body.String(), "Try again in") {
		t.Fatalf("measure inside the window: %d %s", rec.Code, rec.Body.String())
	}
	// The window is per model: once it has passed, Measure works again.
	if _, err := h.store.UpdateSubscriptionCapabilities(ctx, connID, func(rec capability.Record) (bool, error) {
		s := rec["grok-test"]
		s.Probed.At = time.Now().UTC().Add(-planMeasureEvery - time.Second)
		rec["grok-test"] = s
		return true, nil
	}); err != nil {
		t.Fatal(err)
	}
	if rec := h.doAsSession(sess, http.MethodPost, path+"/measure", map[string]any{"model": "grok-test"}); rec.Code != http.StatusAccepted {
		t.Fatalf("measure after the window: %d %s", rec.Code, rec.Body.String())
	}
	for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		c, _ := h.store.SubscriptionByID(ctx, connID)
		if p := c.Capabilities["grok-test"].Probed; p != nil && p.Status != capability.ProbeRunning {
			break
		}
	}
}

// Chat sends an image straight to a plan model whose record says it takes
// images (a built-in default, or the owner's setting) and falls back for a
// plan model nothing is known about.
func TestChatSendsImagesToPlanModelsByRecord(t *testing.T) {
	h, sess := chatHarness(t)
	fake := newFakeXAI(t)
	fake.models = []string{"grok-4.3", "grok-test"}
	h.enableXAISubscriptions(t, sess, true)
	connID, out := h.connectXAI(t, sess)
	if connID == "" {
		t.Fatalf("connect: %v", out)
	}
	if rec := h.selectModels(t, sess, connID, "grok-4.3", "grok-test"); rec.Code != http.StatusOK {
		t.Fatalf("select: %d %s", rec.Code, rec.Body.String())
	}
	lastPlanChat := func() string {
		fake.mu.Lock()
		defer fake.mu.Unlock()
		for i := len(fake.inferenceBody) - 1; i >= 0; i-- {
			if strings.Contains(fake.inferenceBody[i], `"stream":true`) {
				return fake.inferenceBody[i]
			}
		}
		t.Fatal("no chat reached the plan")
		return ""
	}
	send := func(model string) (string, []any) {
		conv := chatConversationFor(t, h, sess, "my/xai/"+model)
		pic := chatUploadOK(t, h, sess, "lake.png", testPNG(t, 2, 2))
		sent := chatSend(t, h, sess, conv, map[string]any{"content": "Describe", "attachment_ids": []string{pic["id"].(string)}})
		chatEvents(t, h, sess, sent["run_id"].(string), "")
		return lastPlanChat(), sent["warnings"].([]any)
	}

	// grok-4.3: assumed for xAI plans.
	body, warnings := send("grok-4.3")
	if !strings.Contains(body, `"image_url"`) || !strings.Contains(body, "data:image/png;base64,") || len(warnings) != 0 {
		t.Fatalf("grok-4.3 should get the image directly: warnings=%v body=%s", warnings, body)
	}
	// grok-test: nothing known, so no direct image.
	body, warnings = send("grok-test")
	if strings.Contains(body, `"image_url"`) || len(warnings) != 1 {
		t.Fatalf("unknown plan model must not get the image: warnings=%v body=%s", warnings, body)
	}
	// The owner says it sees images: now it does.
	if rec := h.doAsSession(sess, http.MethodPatch, "/api/v1/me/subscriptions/"+connID+"/capabilities",
		map[string]any{"model": "grok-test", "set": map[string]any{adapter.CapInImage: true}}); rec.Code != http.StatusOK {
		t.Fatalf("set: %d %s", rec.Code, rec.Body.String())
	}
	body, warnings = send("grok-test")
	if !strings.Contains(body, `"image_url"`) || len(warnings) != 0 {
		t.Fatalf("after the owner set images yes: warnings=%v body=%s", warnings, body)
	}
	// Another person's setting is theirs alone: a second user with the same
	// vendor model and no record still falls back.
	if h.server.chatVision(context.Background(), &store.User{ID: "someone-else"}, "my/xai/grok-test") {
		t.Fatal("a plan model's record leaked to another user")
	}
}
