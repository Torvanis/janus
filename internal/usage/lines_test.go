package usage

import (
	"math/rand"
	"testing"
)

func TestParseLine(t *testing.T) {
	good := []string{
		"price_image_nanousd", "price_image_nanousd@quality=high,size=1024x1024", "price_video_second_nanousd@audio=yes,resolution=1080p",
		"price_mchar_nanousd", "price_audio_minute_nanousd", "price_kpage_nanousd", "price_request_nanousd@tier=flex", "rate_in_nanousd@prompt_over=272000",
	}
	for _, k := range good {
		if l, err := ParseLine(k); err != nil || l.Key() != k {
			t.Errorf("%s: %v (key %q)", k, err, l.Key())
		}
	}
	bad := []string{
		"", "rate_in_nanousd", "price_song_nanousd", "price_image_nanousd@", "price_image_nanousd@quality", "price_image_nanousd@quality=High",
		"price_image_nanousd@size=1024x1024,quality=high", "price_image_nanousd@quality=high,quality=low", "price_image_nanousd@prompt_over=10",
		"rate_in_nanousd@quality=high", "rate_in_nanousd@prompt_over=0", "rate_in_nanousd@prompt_over=abc", "rate_out_nanousd@prompt_over=-5",
		"price_image_nanousd@quality=a b", "price_image_nanousd@quality=" + string(make([]byte, 40)), "context_window",
	}
	for _, k := range bad {
		if _, err := ParseLine(k); err == nil {
			t.Errorf("%q was accepted", k)
		}
	}
}

// A model without long-prompt lines is priced exactly as before lines
// existed, whatever the prompt size: the text-billing fixed point.
func TestForPromptWithoutLinesChangesNothing(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	for i := 0; i < 5000; i++ {
		r := Rates{InNano: rng.Int63n(1e11), OutNano: rng.Int63n(1e11), CachedNano: rng.Int63n(1e10), CacheWrite5mNano: rng.Int63n(1e11), CacheWrite1hNano: rng.Int63n(1e11)}
		c := TokenCounts{In: rng.Int63n(2e6), Out: rng.Int63n(2e5), Cached: rng.Int63n(2e6), CacheWrite5m: rng.Int63n(1e5), CacheWrite1h: rng.Int63n(1e5)}
		want := ComputeCostAll(c, r)
		withUnits := r
		withUnits.Lines = map[string]int64{"price_image_nanousd": 40_000_000, "price_video_second_nanousd@resolution=720p": 100_000_000}
		for _, rates := range []Rates{r, withUnits} {
			if got := ComputeCostAll(c, rates.ForPrompt(c.In)); got != want {
				t.Fatalf("case %d: %d, want %d", i, got, want)
			}
		}
	}
}

func TestForPromptLongContext(t *testing.T) {
	// gpt-6-astra, list prices read 2026-11-06: 10/1/50 per 1M; prompts over 272K: 20/2/75 for the whole request.
	r := Rates{InNano: 10e9, OutNano: 50e9, CachedNano: 1e9, Lines: map[string]int64{
		"rate_in_nanousd@prompt_over=272000": 20e9, "rate_cached_nanousd@prompt_over=272000": 2e9, "rate_out_nanousd@prompt_over=272000": 75e9,
		"rate_in_nanousd@prompt_over=1000000": 40e9,
	}}
	cases := []struct {
		name string
		c    TokenCounts
		want int64
	}{
		{"at the threshold: ordinary", TokenCounts{In: 272_000, Out: 1000}, 272_000*10e9/1e6 + 1000*50e9/1e6},
		{"one over: the whole request at the long rates", TokenCounts{In: 272_001, Out: 1000}, 272_001*20e9/1e6 + 1000*75e9/1e6},
		{"cached tokens count toward the prompt (subset)", TokenCounts{In: 300_000, Cached: 250_000, Out: 10}, 50_000*20e9/1e6 + 250_000*2e9/1e6 + 10*75e9/1e6},
		{"Anthropic/Bedrock: 30k fresh + 250k cache reads is a 280k prompt", TokenCounts{In: 280_000, Cached: 250_000, Out: 10}, 30_000*20e9/1e6 + 250_000*2e9/1e6 + 10*75e9/1e6},
		{"cache writes count toward the prompt", TokenCounts{In: 280_000, CacheWrite5m: 250_000, Out: 10}, 30_000*20e9/1e6 + 10*75e9/1e6},
		{"the highest threshold passed applies; rates it does not name keep the base price", TokenCounts{In: 1_000_001, Out: 10}, 1_000_001*40e9/1e6 + 10*50e9/1e6},
	}
	for _, tc := range cases {
		if got := ComputeCostAll(tc.c, r.ForPrompt(tc.c.In)); got != tc.want {
			t.Errorf("%s: %d, want %d", tc.name, got, tc.want)
		}
	}
	if r.InNano != 10e9 {
		t.Error("ForPrompt changed the rates it was called on")
	}
}

func TestUnitPrice(t *testing.T) {
	r := Rates{Lines: map[string]int64{
		"price_image_nanousd": 40, "price_image_nanousd@quality=high": 170, "price_image_nanousd@quality=high,size=1536x1024": 250,
		"price_video_second_nanousd@resolution=1080p": 500, "rate_in_nanousd@prompt_over=5": 9,
	}}
	for _, tc := range []struct {
		base  string
		attrs map[string]string
		want  int64
		key   string
		ok    bool
	}{
		{LineImage, nil, 40, "price_image_nanousd", true},
		{LineImage, map[string]string{"quality": "low"}, 40, "price_image_nanousd", true},
		{LineImage, map[string]string{"quality": "high", "size": "1024x1024"}, 170, "price_image_nanousd@quality=high", true},
		{LineImage, map[string]string{"quality": "high", "size": "1536x1024"}, 250, "price_image_nanousd@quality=high,size=1536x1024", true},
		{LineVideoSecond, map[string]string{"resolution": "1080p"}, 500, "price_video_second_nanousd@resolution=1080p", true},
		// No line applies: not priced, which is not the same as free.
		{LineVideoSecond, map[string]string{"resolution": "720p"}, 0, "", false},
		{LineCharacters, nil, 0, "", false},
	} {
		got, key, ok := r.UnitPrice(tc.base, tc.attrs)
		if got != tc.want || key != tc.key || ok != tc.ok {
			t.Errorf("%s %v: %d %q %v, want %d %q %v", tc.base, tc.attrs, got, key, ok, tc.want, tc.key, tc.ok)
		}
	}
}

func TestScaled(t *testing.T) {
	r := Rates{InNano: 10, OutNano: 30, Lines: map[string]int64{"rate_in_nanousd@prompt_over=5": 20}}
	half := r.Scaled(0.5)
	if half.InNano != 5 || half.OutNano != 15 || half.Lines["rate_in_nanousd@prompt_over=5"] != 10 || r.Lines["rate_in_nanousd@prompt_over=5"] != 20 {
		t.Errorf("%+v (original %+v)", half, r)
	}
}
