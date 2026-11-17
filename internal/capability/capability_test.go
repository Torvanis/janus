package capability

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/torvanis/janus/internal/adapter"
)

var (
	t1 = time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)
	t2 = t1.Add(time.Hour)
)

func TestSourcesLayerAdminOverProbeOverProvider(t *testing.T) {
	var s State
	s.SetProvider(FromProvider(adapter.Capabilities{adapter.CapInImage: false, adapter.CapTools: true, adapter.CapThinkingLevels: []string{"Low", " high ", "low"}}, t1))
	s.Probe = Facts{adapter.CapInImage: {Value: true, At: t2, Note: "the model described the picture"}}
	if err := s.SetAdmin(map[string]any{adapter.CapTools: false}, t2); err != nil {
		t.Fatal(err)
	}
	r := s.Resolve()
	if r[adapter.CapInImage].Value != true || r[adapter.CapInImage].Source != SourceProbe || r[adapter.CapInImage].Note == "" {
		t.Errorf("a measurement must beat a description: %+v", r[adapter.CapInImage])
	}
	if r[adapter.CapTools].Value != false || r[adapter.CapTools].Source != SourceAdmin {
		t.Errorf("an override must beat everything: %+v", r[adapter.CapTools])
	}
	if got := r[adapter.CapThinkingLevels]; !reflect.DeepEqual(got.Value, []string{"low", "high"}) || got.Source != SourceProvider {
		t.Errorf("levels: %+v", got)
	}
	if _, ok := r[adapter.CapInAudio]; ok {
		t.Error("a key no source spoke to must be absent (unknown), not false")
	}
	// Clearing the override brings the provider's value back.
	if err := s.SetAdmin(map[string]any{adapter.CapTools: nil}, t2); err != nil || s.Resolve()[adapter.CapTools].Source != SourceProvider || s.Admin != nil {
		t.Errorf("clear: %v %+v", err, s.Resolve()[adapter.CapTools])
	}

	p := s.Profile()
	if v, known := p.Bool(adapter.CapInImage); !v || !known {
		t.Error("profile bool")
	}
	if _, known := p.Bool(adapter.CapInAudio); known {
		t.Error("unknown bool reported as known")
	}
	if l, known := p.List(adapter.CapThinkingLevels); !known || len(l) != 2 {
		t.Error("profile list")
	}
	if _, known := p.List(adapter.CapRefusedParams); known {
		t.Error("unknown list reported as known")
	}
}

func TestStateSurvivesStorage(t *testing.T) {
	var s State
	s.SetProvider(FromProvider(adapter.Capabilities{adapter.CapMaxOutput: int64(64000), adapter.CapThinkingLevels: []string{"low"}, adapter.CapOutputCap: adapter.OutputCapMaxTokens, adapter.CapThinking: true}, t1))
	s.Probed = &ProbeRun{At: t2, Status: ProbeOK, Requests: 6, InputTokens: 120, OutputTokens: 40, By: "enabled"}
	back, err := Parse(s.String())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(back.Resolve(), s.Resolve()) {
		t.Errorf("round trip changed values:\n got %#v\nwant %#v", back.Resolve(), s.Resolve())
	}
	if back.Profile().Int(adapter.CapMaxOutput) != 64000 || back.Probed == nil || back.Probed.Requests != 6 {
		t.Errorf("typed values lost: %+v", back)
	}
	if empty, err := Parse(""); err != nil || empty.Profile().Known() {
		t.Error("empty state")
	}
	if _, err := Parse("{not json"); err == nil {
		t.Error("malformed state accepted")
	}
	// A key this version does not know is dropped, not an error.
	if got, err := Parse(`{"provider":{"input.smell":{"value":true,"at":"2026-11-12T10:00:00Z"},"input.image":{"value":true,"at":"2026-11-12T10:00:00Z"}}}`); err != nil || len(got.Provider) != 1 {
		t.Errorf("unknown key: %v %+v", err, got.Provider)
	}
}

func TestProviderFactsKeepTheirDateUntilTheyChange(t *testing.T) {
	var s State
	if !s.SetProvider(FromProvider(adapter.Capabilities{adapter.CapInImage: true}, t1)) {
		t.Error("first write must report a change")
	}
	if s.SetProvider(FromProvider(adapter.Capabilities{adapter.CapInImage: true}, t2)) {
		t.Error("the same facts are not a change")
	}
	if !s.Provider[adapter.CapInImage].At.Equal(t1) {
		t.Error("an unchanged fact keeps the date it was first seen")
	}
	if !s.SetProvider(FromProvider(adapter.Capabilities{adapter.CapInImage: false}, t2)) || !s.Provider[adapter.CapInImage].At.Equal(t2) {
		t.Error("a changed fact takes the new date")
	}
	if !s.SetProvider(nil) || s.Provider != nil {
		t.Error("a provider that stops saying anything clears its facts")
	}
}

func TestValuesAreChecked(t *testing.T) {
	// A provider's list is outside input: wrong types and unknown keys are dropped.
	got := FromProvider(adapter.Capabilities{adapter.CapInImage: "yes", "made.up": true, adapter.CapThinkingOff: "sometimes", adapter.CapMaxOutput: int64(-5),
		adapter.CapThinkingLevels: []any{"low", 3}, adapter.CapTools: true}, t1)
	if len(got) != 1 || got[adapter.CapTools].Value != true {
		t.Errorf("unchecked provider facts: %#v", got)
	}
	var s State
	tooMany := []string{}
	for i := 0; i < 40; i++ {
		tooMany = append(tooMany, "level-"+strings.Repeat("x", i+1))
	}
	for _, bad := range []map[string]any{
		{"made.up": true}, {adapter.CapInImage: "true"}, {adapter.CapOutputCap: "max_length"}, {adapter.CapMaxOutput: 1.5}, {adapter.CapThinkingLevels: "low"}, {"made.up": nil},
		{adapter.CapThinkingLevels: tooMany},
	} {
		if err := s.SetAdmin(bad, t1); err == nil {
			t.Errorf("accepted %v", bad)
		}
	}
	if s.Admin != nil {
		t.Errorf("a refused edit changed the state: %#v", s.Admin)
	}
	// JSON numbers and lists arrive as float64 and []any.
	if err := s.SetAdmin(map[string]any{adapter.CapMaxOutput: float64(8192), adapter.CapRefusedParams: []any{"Temperature", "top_p"}, adapter.CapThinkingOff: "NEVER"}, t1); err != nil {
		t.Fatal(err)
	}
	p := s.Profile()
	if l, _ := p.List(adapter.CapRefusedParams); p.Int(adapter.CapMaxOutput) != 8192 || !reflect.DeepEqual(l, []string{"temperature", "top_p"}) || p.Text(adapter.CapThinkingOff) != adapter.ThinkingOffNever {
		t.Errorf("normalized admin values: %#v", s.Admin)
	}
}
