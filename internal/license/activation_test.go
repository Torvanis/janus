package license

import (
	"strings"
	"testing"
)

func TestActivationRoundTrip(t *testing.T) {
	code := EncodeActivation(Prefix+".payload.sig", "abc123")
	if !strings.HasPrefix(code, ActivationPrefix+".") || strings.ContainsAny(code, " \n") {
		t.Fatalf("bad code %q", code)
	}
	a, err := ParseActivation("  " + code + "\n")
	if err != nil || a.License != Prefix+".payload.sig" || a.SyncToken != "abc123" {
		t.Fatalf("round trip: %+v %v", a, err)
	}
}

func TestActivationRejects(t *testing.T) {
	for name, code := range map[string]string{
		"license":   Prefix + ".x.y",
		"prefix":    "JANUS-ACTIVATION-2.e30",
		"base64":    ActivationPrefix + ".!!!",
		"json":      ActivationPrefix + ".bm90anNvbg",
		"nolicense": EncodeActivation("", "tok"),
		"nottoken":  EncodeActivation(Prefix+".a.b", ""),
		"dotted":    EncodeActivation(Prefix+".a.b", "JNS-COM-AAAA-BBBB.tok"),
	} {
		if _, err := ParseActivation(code); err == nil {
			t.Errorf("%s: accepted %q", name, code)
		}
	}
}
