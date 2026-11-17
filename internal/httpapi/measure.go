package httpapi

import (
	"encoding/json"
	"math"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/torvanis/janus/internal/usage"
)

// Measuring requests billed by something other than tokens.
//
// Images are billed per image, video per second, text-to-speech per
// character. Providers rarely say how many they billed, so Janus measures it:
// from the response where it says (the images it returned), else from the
// request (the characters sent, the seconds asked for), else the provider's
// documented default. The measurement is priced in applyUsage with the
// model's price line for the unit; without a line it is recorded as measured
// and unpriced.

// videoDefaultSeconds is each provider's documented length of a video when
// the request does not give one.
var videoDefaultSeconds = map[string]float64{
	"xai":    8, // docs.x.ai videos: duration 1-15, default 8 (extensions 2-10, default 6)
	"openai": 4, // Sora: seconds 4, 8 or 12, default 4
	"google": 8, // Veo: durationSeconds, default 8
}

// providersBillingVideoOnResult report a video's cost on the request that
// fetches the finished job (xAI's usage.cost_in_usd_ticks), so the request
// that starts it must not be billed as well.
var providersBillingVideoOnResult = map[string]bool{"xai": true}

// measureRequest reads what a request will be billed by from what the
// caller sent. provider is adapter.MetadataProvider's name for the upstream
// ("" when unknown). It returns nil for a request billed by tokens, and
// statusCheck for a request that only asks how a provider job is going or
// downloads its result: those run nothing and are never billed themselves.
func measureRequest(method, path string, body []byte, provider string) (m *usage.UnitUse, statusCheck, onResult bool) {
	p := strings.TrimRight(path, "/")
	switch {
	case strings.HasSuffix(p, "/audio/speech") && method == http.MethodPost:
		var req struct {
			Input string `json:"input"`
		}
		if json.Unmarshal(body, &req) != nil || req.Input == "" {
			return nil, false, false
		}
		return &usage.UnitUse{Base: usage.LineCharacters, Quantity: float64(utf8.RuneCountInString(req.Input)), Source: usage.MeasureRequest}, false, false

	case (strings.HasSuffix(p, "/images/generations") || strings.HasSuffix(p, "/images/edits") || strings.HasSuffix(p, "/images/variations")) && method == http.MethodPost:
		// Edits and variations arrive as multipart forms; only n, size and
		// quality are read, and a form simply leaves them unset.
		var req struct {
			N          json.RawMessage `json:"n"`
			Size       string          `json:"size"`
			Quality    string          `json:"quality"`
			Resolution string          `json:"resolution"` // xAI: 1k, 1.5k, 2k
		}
		_ = json.Unmarshal(body, &req)
		n, src := 1.0, usage.MeasureDefault
		if v, ok := jsonNumber(req.N); ok && v >= 1 {
			n, src = v, usage.MeasureRequest
		}
		return &usage.UnitUse{Base: usage.LineImage, Quantity: n, Attrs: attrs("size", req.Size, "quality", req.Quality, "resolution", req.Resolution), Source: src}, false, false

	case isVideoPath(p):
		if method != http.MethodPost {
			return nil, true, false
		}
		var req struct {
			Duration      json.RawMessage `json:"duration"`
			Seconds       json.RawMessage `json:"seconds"`
			Resolution    string          `json:"resolution"`
			Size          string          `json:"size"`
			GenerateAudio *bool           `json:"generate_audio"`
		}
		_ = json.Unmarshal(body, &req)
		secs, src := 0.0, usage.MeasureRequest
		if v, ok := jsonNumber(req.Duration); ok && v > 0 {
			secs = v
		} else if v, ok := jsonNumber(req.Seconds); ok && v > 0 {
			secs = v
		} else if d, known := videoDefaultSeconds[provider]; known {
			secs, src = d, usage.MeasureDefault
			if provider == "xai" && strings.HasSuffix(p, "/extensions") {
				secs = 6
			}
		}
		if secs == 0 {
			return nil, false, false
		}
		res := strings.ToLower(strings.TrimSpace(req.Resolution))
		if res == "" {
			res = resolutionOfSize(req.Size)
		}
		audio := ""
		if req.GenerateAudio != nil {
			audio = map[bool]string{true: "yes", false: "no"}[*req.GenerateAudio]
		}
		return &usage.UnitUse{Base: usage.LineVideoSecond, Quantity: secs, Attrs: attrs("resolution", res, "audio", audio), Source: src},
			false, providersBillingVideoOnResult[provider]
	}
	return nil, false, false
}

// isVideoPath reports a video endpoint: /v1/videos, /v1/videos/generations,
// /v1/videos/{id}, /v1/videos/{id}/content, Veo's operations.
func isVideoPath(p string) bool {
	return strings.HasSuffix(p, "/videos") || strings.Contains(p, "/videos/")
}

// refineFromResponse corrects a measurement with what the provider's
// (OpenAI-shaped) response says: the number of images it returned.
func refineFromResponse(m *usage.UnitUse, payload []byte) {
	if m == nil || m.Base != usage.LineImage {
		return
	}
	var resp struct {
		Data []json.RawMessage `json:"data"`
	}
	if json.Unmarshal(payload, &resp) == nil && len(resp.Data) > 0 {
		m.Quantity, m.Source = float64(len(resp.Data)), usage.MeasureResponse
	}
}

// jsonNumber reads a number sent as a JSON number or a numeric string.
func jsonNumber(raw json.RawMessage) (float64, bool) {
	if len(raw) == 0 {
		return 0, false
	}
	var f float64
	if json.Unmarshal(raw, &f) == nil {
		return f, !math.IsNaN(f) && !math.IsInf(f, 0)
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		if f, err := strconv.ParseFloat(strings.TrimSpace(s), 64); err == nil && !math.IsNaN(f) && !math.IsInf(f, 0) {
			return f, true
		}
	}
	return 0, false
}

// resolutionOfSize turns "1280x720" into "720p" (the short side).
func resolutionOfSize(size string) string {
	w, h, ok := strings.Cut(strings.ToLower(strings.TrimSpace(size)), "x")
	if !ok {
		return ""
	}
	a, err1 := strconv.Atoi(w)
	b, err2 := strconv.Atoi(h)
	if err1 != nil || err2 != nil || a <= 0 || b <= 0 {
		return ""
	}
	return strconv.Itoa(min(a, b)) + "p"
}

// attrs builds a condition map from name/value pairs, lower-cased, leaving
// out values that are empty or could not be a condition value.
func attrs(kv ...string) map[string]string {
	out := map[string]string{}
	for i := 0; i+1 < len(kv); i += 2 {
		v := strings.ToLower(strings.TrimSpace(kv[i+1]))
		if v != "" && len(v) <= 32 {
			out[kv[i]] = v
		}
	}
	return out
}
