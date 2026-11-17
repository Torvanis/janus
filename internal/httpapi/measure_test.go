package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/torvanis/janus/internal/store"
	"github.com/torvanis/janus/internal/usage"
)

// Requests billed by something other than tokens are measured (characters
// read aloud, images made, seconds of video) and priced with the model's
// price line for the unit. Without a line they are recorded as measured and
// unpriced; a provider job's status checks are never billed.
func TestMeasuredUnitsArePricedByTheirLines(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	admin := adminSession(t, h)

	var polls atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/audio/speech":
			w.Header().Set("Content-Type", "audio/mpeg")
			_, _ = w.Write(make([]byte, 4096))
		case r.URL.Path == "/v1/images/generations":
			// The caller asked for one; the provider returned two.
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"created":1,"data":[{"b64_json":"aGk="},{"b64_json":"aGk="}]}`))
		case r.URL.Path == "/v1/videos" && r.Method == http.MethodPost:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"vid_1","object":"video","status":"queued"}`))
		case r.URL.Path == "/v1/videos/vid_1":
			w.Header().Set("Content-Type", "application/json")
			switch polls.Add(1) {
			case 1:
				_, _ = w.Write([]byte(`{"id":"vid_1","object":"video","status":"in_progress","progress":40}`))
			case 2:
				// Token counts on a status check.
				_, _ = w.Write([]byte(`{"id":"vid_1","status":"in_progress","usage":{"prompt_tokens":900,"completion_tokens":900}}`))
			default:
				// xAI reports the job's cost on the finished job.
				_, _ = w.Write([]byte(`{"id":"vid_1","status":"done","video":{"url":"https://v.example/1.mp4","duration":1},"usage":{"cost_in_usd_ticks":500000000}}`))
			}
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(up.Close)
	if err := h.store.UpdateUpstream(ctx, h.model.UpstreamID, up.URL, true, "", ""); err != nil {
		t.Fatal(err)
	}

	seen := 0
	next := func(method, path string, body any) (*store.UsageEvent, *httptest.ResponseRecorder) {
		t.Helper()
		rec := h.do(method, path, body)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s %s: %d %s", method, path, rec.Code, rec.Body.String())
		}
		seen++
		deadline := time.Now().Add(30 * time.Second)
		for time.Now().Before(deadline) {
			evs, total, _ := h.store.ListRequests(ctx, store.RequestFilter{Limit: 1})
			if total >= seen && len(evs) == 1 {
				return evs[0], rec
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatalf("no usage row for %s %s", method, path)
		return nil, nil
	}
	speech := map[string]any{"model": "test-model", "input": "Hello from Janus.", "voice": "alloy"} // 17 characters

	// No line yet: measured, unpriced, at no cost. Not "free", not a gap in metering.
	ev, _ := next(http.MethodPost, "/v1/audio/speech", speech)
	if ev.AccountingMode != usage.AccountingMeasuredUnpriced || ev.BilledUnit != "character" || ev.BilledQuantity != 17 || ev.CostNano != 0 || ev.BilledLine != "" {
		t.Fatalf("speech without a line: %s %s %v cost %d line %q", ev.AccountingMode, ev.BilledUnit, ev.BilledQuantity, ev.CostNano, ev.BilledLine)
	}

	rec := h.doAsSession(admin, http.MethodPatch, "/api/v1/admin/models/"+h.model.ID, map[string]any{"price_lines": map[string]any{
		"price_mchar_nanousd":                                  15,   // $15 per million characters
		"price_image_nanousd":                                  0.04, // $0.04 an image
		"price_image_nanousd@quality=high":                     0.17,
		"price_video_second_nanousd@resolution=720p":           0.10,
		"price_video_second_nanousd@audio=yes,resolution=720p": 0.15,
	}})
	if rec.Code != http.StatusOK {
		t.Fatalf("lines: %d %s", rec.Code, rec.Body.String())
	}
	h.server.InvalidateConfigCache()

	ev, _ = next(http.MethodPost, "/v1/audio/speech", speech)
	if ev.AccountingMode != usage.AccountingMeasured || ev.CostNano != 17*15_000_000_000/1_000_000 || ev.BilledLine != "price_mchar_nanousd" {
		t.Errorf("speech: %s cost %d line %q", ev.AccountingMode, ev.CostNano, ev.BilledLine)
	}

	// The provider's count wins over the request's, and the most specific line applies.
	ev, rec = next(http.MethodPost, "/v1/images/generations", map[string]any{"model": "test-model", "prompt": "a cat", "n": 1, "quality": "high"})
	if ev.AccountingMode != usage.AccountingMeasured || ev.BilledUnit != "image" || ev.BilledQuantity != 2 || ev.CostNano != 340_000_000 || ev.BilledLine != "price_image_nanousd@quality=high" {
		t.Errorf("images: %s %v cost %d line %q", ev.AccountingMode, ev.BilledQuantity, ev.CostNano, ev.BilledLine)
	}
	if got := rec.Header().Get("X-Janus-Cost-USD"); got != "0.34" {
		t.Errorf("cost header = %q, want the recorded cost", got)
	}

	// Video: seconds and resolution from the request, sound when it says.
	ev, _ = next(http.MethodPost, "/v1/videos", map[string]any{"model": "test-model", "prompt": "a cat", "seconds": "8", "size": "1280x720"})
	if ev.AccountingMode != usage.AccountingMeasured || ev.BilledUnit != "video_second" || ev.BilledQuantity != 8 || ev.CostNano != 800_000_000 {
		t.Errorf("video: %s %s %v cost %d", ev.AccountingMode, ev.BilledUnit, ev.BilledQuantity, ev.CostNano)
	}
	ev, _ = next(http.MethodPost, "/v1/videos", map[string]any{"model": "test-model", "prompt": "a cat", "seconds": 4, "size": "720x1280", "generate_audio": true})
	if ev.CostNano != 600_000_000 || ev.BilledLine != "price_video_second_nanousd@audio=yes,resolution=720p" {
		t.Errorf("video with sound: cost %d line %q", ev.CostNano, ev.BilledLine)
	}
	// A resolution with no line: measured, unpriced.
	ev, _ = next(http.MethodPost, "/v1/videos", map[string]any{"model": "test-model", "prompt": "a cat", "seconds": 4, "size": "1920x1080"})
	if ev.AccountingMode != usage.AccountingMeasuredUnpriced || ev.CostNano != 0 {
		t.Errorf("unpriced resolution: %s cost %d", ev.AccountingMode, ev.CostNano)
	}

	// Asking how the job is going runs nothing: not billable, not a metering gap.
	ev, _ = next(http.MethodGet, "/v1/videos/vid_1", nil)
	if ev.AccountingMode != usage.AccountingNone || ev.CostNano != 0 || ev.BilledUnit != "" {
		t.Errorf("status check: %s cost %d unit %q", ev.AccountingMode, ev.CostNano, ev.BilledUnit)
	}

	// The finished job carries the provider's cost: billed there, once.
	// Token counts on a status check are never billed.
	ev, _ = next(http.MethodGet, "/v1/videos/vid_1", nil)
	if ev.AccountingMode != usage.AccountingNone || ev.CostNano != 0 || ev.TokensIn != 0 {
		t.Errorf("status check with tokens: %s cost %d in %d", ev.AccountingMode, ev.CostNano, ev.TokensIn)
	}
	ev, _ = next(http.MethodGet, "/v1/videos/vid_1", nil)
	if ev.AccountingMode != usage.AccountingUpstreamCost || ev.CostNano != 50_000_000 {
		t.Errorf("finished job with a reported cost: %s cost %d", ev.AccountingMode, ev.CostNano)
	}
	if ev.EstimatedCostNano != nil {
		// The job was measured where it started; its result has nothing
		// of its own to price, so no estimate rather than a false $0.
		t.Errorf("finished job result carries an estimate: %d", *ev.EstimatedCostNano)
	}

	// The System page's metering tile names what needs a price.
	sum, err := h.store.AccountingSummarySince(ctx, time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if sum.MeasuredUnpriced != 2 || sum.Measured != 4 || len(sum.Gaps) != 2 {
		t.Errorf("summary: %+v", sum)
	}
	for _, g := range sum.Gaps {
		if g.Accounting != usage.AccountingMeasuredUnpriced || !strings.Contains("tts video", g.Modality) {
			t.Errorf("gap: %+v", g)
		}
	}
}

func TestMeasureRequest(t *testing.T) {
	for _, tc := range []struct {
		name, method, path, body, provider string
		base                               string
		qty                                float64
		source                             string
		attrs                              map[string]string
		status, onResult                   bool
	}{
		{name: "speech counts characters, not bytes", method: "POST", path: "/v1/audio/speech", body: `{"input":"héllo"}`, base: usage.LineCharacters, qty: 5, source: usage.MeasureRequest, attrs: nil},
		{name: "images default to one", method: "POST", path: "/v1/images/generations", body: `{"prompt":"x","size":"1024x1024"}`, base: usage.LineImage, qty: 1, source: usage.MeasureDefault, attrs: map[string]string{"size": "1024x1024"}},
		{name: "xAI image tier", method: "POST", path: "/v1/images/generations", body: `{"quality":"low","resolution":"1k"}`, base: usage.LineImage, qty: 1, source: usage.MeasureDefault, attrs: map[string]string{"quality": "low", "resolution": "1k"}},
		{name: "images n as a string", method: "POST", path: "/v1/images/generations", body: `{"n":"3","quality":"HIGH"}`, base: usage.LineImage, qty: 3, source: usage.MeasureRequest, attrs: map[string]string{"quality": "high"}},
		{name: "xAI video: duration and resolution, billed on the result", method: "POST", path: "/v1/videos/generations", body: `{"duration":6,"resolution":"720p"}`, provider: "xai", base: usage.LineVideoSecond, qty: 6, source: usage.MeasureRequest, attrs: map[string]string{"resolution": "720p"}, onResult: true},
		{name: "xAI video default length", method: "POST", path: "/v1/videos/generations", body: `{}`, provider: "xai", base: usage.LineVideoSecond, qty: 8, source: usage.MeasureDefault, attrs: map[string]string{}, onResult: true},
		{name: "xAI extension default length", method: "POST", path: "/v1/videos/extensions", body: `{}`, provider: "xai", base: usage.LineVideoSecond, qty: 6, source: usage.MeasureDefault, attrs: map[string]string{}, onResult: true},
		{name: "Veo default length", method: "POST", path: "/v1/videos", body: `{"size":"1280x720"}`, provider: "google", base: usage.LineVideoSecond, qty: 8, source: usage.MeasureDefault, attrs: map[string]string{"resolution": "720p"}},
		{name: "unknown provider without a length is not measured", method: "POST", path: "/v1/videos", body: `{}`},
		{name: "polling a job", method: "GET", path: "/v1/videos/abc", status: true},
		{name: "downloading a result", method: "GET", path: "/v1/videos/models/veo/operations/x/content", status: true},
		{name: "chat is billed by tokens", method: "POST", path: "/v1/chat/completions", body: `{"messages":[]}`},
	} {
		m, status, onResult := measureRequest(tc.method, tc.path, []byte(tc.body), tc.provider)
		if status != tc.status || onResult != tc.onResult {
			t.Errorf("%s: status %v onResult %v", tc.name, status, onResult)
		}
		if tc.base == "" {
			if m != nil {
				t.Errorf("%s: measured %+v", tc.name, m)
			}
			continue
		}
		if m == nil || m.Base != tc.base || m.Quantity != tc.qty || m.Source != tc.source || len(m.Attrs) != len(tc.attrs) {
			t.Errorf("%s: %+v", tc.name, m)
			continue
		}
		for k, v := range tc.attrs {
			if m.Attrs[k] != v {
				t.Errorf("%s: attr %s = %q", tc.name, k, m.Attrs[k])
			}
		}
	}
}
