package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/torvanis/janus/internal/secgw/classify"
	"github.com/torvanis/janus/internal/store"
	"github.com/torvanis/janus/internal/usage"
)

// teiServer is a fake TEI /predict classifier that counts calls and records
// the Authorization header it last saw.
func teiServer(t *testing.T, calls *atomic.Int32, auth *atomic.Value) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if auth != nil {
			auth.Store(r.Header.Get("Authorization"))
		}
		var req struct {
			Inputs [][]string `json:"inputs"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		out := make([][]map[string]any, len(req.Inputs))
		for i := range req.Inputs {
			out[i] = []map[string]any{{"label": "BENIGN", "score": 0.98}, {"label": "MALICIOUS", "score": 0.02}}
		}
		_ = json.NewEncoder(w).Encode(out)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func w7GuardModel(t *testing.T, h *harness, baseURL, encKey string) (*store.Upstream, *store.Model) {
	t.Helper()
	ctx := context.Background()
	up, err := h.store.CreateUpstream(ctx, "w7-guard", "openai_compatible", baseURL, encKey, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.store.UpsertDiscoveredModel(ctx, up.ID, "prompt-guard-2", []string{"chat"}); err != nil {
		t.Fatal(err)
	}
	models, _ := h.store.ListModels(ctx, store.ModelFilter{UpstreamID: up.ID})
	m := models[0]
	_ = h.store.SetModelStatus(ctx, m.ID, store.ModelEnabled)
	if err := h.store.SetModelClassifierRole(ctx, m.ID, store.ClassifierRoleTextClassification); err != nil {
		t.Fatal(err)
	}
	return up, m
}

// Rotating the base URL or key of a classifier's upstream must reach the
// cached classifier client without a restart (once the config cache
// expires, simulated here by an invalidation without touching the
// resolver).
func TestClassifierClientFollowsUpstreamRotation(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	var oldCalls, newCalls atomic.Int32
	var auth atomic.Value
	oldSrv := teiServer(t, &oldCalls, nil)
	newSrv := teiServer(t, &newCalls, &auth)
	encOld, _ := h.server.Cipher.Encrypt("old-key")
	up, m := w7GuardModel(t, h, oldSrv.URL, encOld)

	seg := []classify.Segment{{Index: 0, Text: "hello"}}
	c1, err := h.server.secgwClassifiers().For(ctx, m.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c1.Classify(ctx, seg); err != nil {
		t.Fatal(err)
	}
	encNew, _ := h.server.Cipher.Encrypt("new-key")
	if err := h.store.UpdateUpstream(ctx, up.ID, newSrv.URL, true, encNew, "new-…"); err != nil {
		t.Fatal(err)
	}
	h.server.InvalidateConfigCache() // the TTL running out on another replica
	c2, err := h.server.secgwClassifiers().For(ctx, m.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c2.Classify(ctx, seg); err != nil {
		t.Fatal(err)
	}
	if newCalls.Load() != 1 || oldCalls.Load() != 1 {
		t.Fatalf("calls old=%d new=%d, want 1 and 1: the classifier kept the old base URL", oldCalls.Load(), newCalls.Load())
	}
	if got, _ := auth.Load().(string); got != "Bearer new-key" {
		t.Fatalf("authorization = %q, want the rotated key", got)
	}
}

// A classifier whose catalog model is disabled or stale is no longer called.
func TestClassifierDisabledOrStaleModelIsNotCalled(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	var calls atomic.Int32
	srv := teiServer(t, &calls, nil)
	up, m := w7GuardModel(t, h, srv.URL, "")
	if _, err := h.server.secgwClassifiers().For(ctx, m.ID); err != nil {
		t.Fatal(err)
	}
	for _, status := range []string{store.ModelDisabled, store.ModelStale} {
		if status == store.ModelStale {
			_ = h.store.SetModelStatus(ctx, m.ID, store.ModelEnabled)
			if _, err := h.store.MarkStaleModels(ctx, up.ID, []string{"something-else"}); err != nil {
				t.Fatal(err)
			}
		} else if err := h.store.SetModelStatus(ctx, m.ID, status); err != nil {
			t.Fatal(err)
		}
		h.server.InvalidateConfigCache()
		_, err := h.server.secgwClassifiers().For(ctx, m.ID)
		if err == nil || !strings.Contains(err.Error(), "is "+status+" in the model catalog") {
			t.Fatalf("status %s: For error = %v, want a not-available error", status, err)
		}
	}
	if calls.Load() != 0 {
		t.Fatalf("classifier called %d times", calls.Load())
	}
}

// replica builds a second Server on the same database: another HA replica,
// with its own in-process caches, breakers and throttles.
func (h *harness) replica() *Server {
	return &Server{Config: h.server.Config, Store: h.store, Cipher: h.server.Cipher, Quota: h.server.Quota,
		Metrics: h.server.Metrics, Alerts: h.server.Alerts, Logger: h.server.Logger, StartedAt: time.Now()}
}

func countTitles(t *testing.T, h *harness, userID, substr string) int {
	t.Helper()
	notes, _, err := h.store.ListNotifications(context.Background(), userID, false, 200)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, x := range notes {
		if strings.Contains(x.Title, substr) {
			n++
		}
	}
	return n
}

func eventually(t *testing.T, cond func() bool, msg func() string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal(msg())
}

// A classifier outage seen by two replicas, each breaker re-opening after
// its cooldown, is announced once; its recovery once.
func TestClassifierBreakerAlertsOncePerOutageAcrossReplicas(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	var down atomic.Bool
	down.Store(true)
	var calls atomic.Int32
	inner := teiServer(t, &calls, nil)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if down.Load() {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		inner.Config.Handler.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	_, m := w7GuardModel(t, h, srv.URL, "")
	seg := []classify.Segment{{Index: 0, Text: "hello"}}
	var breakers []*classify.Breaker
	for _, s := range []*Server{h.server, h.replica()} {
		c, err := s.secgwClassifiers().For(ctx, m.ID)
		if err != nil {
			t.Fatal(err)
		}
		b := c.(*classify.Breaker)
		b.Cooldown = time.Millisecond
		breakers = append(breakers, b)
	}
	// Three outage cycles on each replica: each opens its breaker three times.
	for cycle := 0; cycle < 3; cycle++ {
		for _, b := range breakers {
			for i := 0; i < 3; i++ {
				_, _ = b.Classify(ctx, seg)
			}
			time.Sleep(3 * time.Millisecond)
		}
	}
	unavailable := func() int { return countTitles(t, h, h.user.ID, "Security classifier unavailable") }
	eventually(t, func() bool { return unavailable() >= 1 }, func() string { return "no unavailable alert" })
	time.Sleep(200 * time.Millisecond) // let any duplicate OnOpen goroutines land
	if got := unavailable(); got != 1 {
		t.Fatalf("unavailable alerts = %d for one outage on two replicas, want 1", got)
	}
	down.Store(false)
	for _, b := range breakers {
		if _, err := b.Classify(ctx, seg); err != nil {
			t.Fatal(err)
		}
	}
	recovered := func() int { return countTitles(t, h, h.user.ID, "Security classifier recovered") }
	eventually(t, func() bool { return recovered() >= 1 }, func() string { return "no recovery alert" })
	time.Sleep(200 * time.Millisecond)
	if got := recovered(); got != 1 {
		t.Fatalf("recovery alerts = %d, want 1", got)
	}
}

// The Child Safety "blocked" throttle (one notice per child per 15 min)
// holds across replicas, not per replica.
func TestFamilyBlockedNoticeThrottledAcrossReplicas(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	n := &familyNotice{userID: h.user.ID, action: store.SecgwActionBlocked}
	h.server.notifyFamily(ctx, n)
	h.replica().notifyFamily(ctx, n)
	h.replica().notifyFamily(ctx, n)
	if got := countTitles(t, h, h.user.ID, "Child Safety blocked a message"); got != 1 {
		t.Fatalf("blocked notices = %d from three replicas within the window, want 1", got)
	}
	// Support notices are never throttled.
	sup := &familyNotice{userID: h.user.ID, action: store.SecgwActionSupported}
	h.server.notifyFamily(ctx, sup)
	h.replica().notifyFamily(ctx, sup)
	if got := countTitles(t, h, h.user.ID, "may need support"); got != 2 {
		t.Fatalf("support notices = %d, want 2", got)
	}
}

// Revoking a service token on one replica must stop it on the others at
// once, not after their 60 s token cache expires; so must shortening its
// expiry into the past.
func TestServiceTokenRevokeIsImmediateOnOtherReplicas(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	other := h.replica()
	for _, revoke := range []bool{true, false} {
		svc, plain, err := h.store.CreateServiceToken(ctx, "site-"+strconv.FormatBool(revoke), "", h.user.ID, time.Time{})
		if err != nil {
			t.Fatal(err)
		}
		// Warm the other replica's cache.
		if _, err := other.resolveServiceToken(ctx, plain); err != nil {
			t.Fatal(err)
		}
		if revoke {
			if err := h.store.RevokeServiceToken(ctx, svc.ID); err != nil {
				t.Fatal(err)
			}
		} else if err := h.store.UpdateServiceToken(ctx, svc.ID, svc.Name, "", time.Now().Add(-time.Minute)); err != nil {
			t.Fatal(err)
		}
		if _, err := other.resolveServiceToken(ctx, plain); err == nil {
			t.Fatalf("revoke=%v: the other replica still accepts the service token from its cache", revoke)
		}
	}
}

// A generative guard (Llama Guard on a paid chat API) that reports token
// usage is priced from its rate card, like a decision classifier.
func TestGuardClassifierCallIsPricedFromRateCard(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	guard := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"safe"}}],"usage":{"prompt_tokens":1000,"completion_tokens":2}}`))
	}))
	t.Cleanup(guard.Close)
	up, err := h.store.CreateUpstream(ctx, "paid-guard", "openai_compatible", guard.URL, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.store.UpsertDiscoveredModel(ctx, up.ID, "llama-guard-4", []string{"chat"}); err != nil {
		t.Fatal(err)
	}
	models, _ := h.store.ListModels(ctx, store.ModelFilter{UpstreamID: up.ID})
	lg := models[0]
	_ = h.store.SetModelStatus(ctx, lg.ID, store.ModelEnabled)
	if err := h.store.SetModelClassifierRole(ctx, lg.ID, store.ClassifierRoleGenerativeGuard); err != nil {
		t.Fatal(err)
	}
	if err := h.store.SetModelRates(ctx, lg.ID, store.RateCard{
		RateInNano: 1 * usage.NanoPerUSD, RateOutNano: 1 * usage.NanoPerUSD,
		EffectiveFrom: time.Now().UTC().Add(-time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	h.server.InvalidateConfigCache()
	c, err := h.server.secgwClassifiers().For(ctx, lg.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.(classify.ConversationGuard).Judge(ctx, []classify.Turn{{Role: "user", Content: "hi"}}); err != nil {
		t.Fatal(err)
	}
	var ev *store.UsageEvent
	for _, e := range h.waitForUsageEvents(1) {
		if e.ModelID == lg.ID {
			ev = e
		}
	}
	if ev == nil {
		t.Fatal("no usage event for the guard call")
	}
	// Any non-zero cost and a billable accounting mode prove the rate card
	// applied (the old meter wrote not_billable with cost 0).
	if ev.TokensIn != 1000 || ev.TokensOut != 2 || ev.CostNano <= 0 || ev.AccountingMode == usage.AccountingNone {
		t.Fatalf("guard call event: tokens %d/%d cost %d status %q mode %q; want priced from the rate card",
			ev.TokensIn, ev.TokensOut, ev.CostNano, ev.CostStatus, ev.AccountingMode)
	}
}
