package scim

import (
	"encoding/json"
	"errors"
	"net/url"
	"strings"
	"testing"
)

func TestFilter(t *testing.T) {
	r := map[string]any{"userName": "Alice", "active": true, "age": float64(12), "emails": []any{map[string]any{"type": "work", "value": "a@example.com"}, map[string]any{"type": "home", "value": "b@home.com"}}}
	for _, tc := range []struct {
		q    string
		want bool
	}{
		{`USERNAME eq "alice"`, true}, {`userName ne "Bob"`, true}, {`userName co "LIC"`, true}, {`userName sw "Al"`, true}, {`userName ew "CE"`, true}, {`userName pr`, true}, {`missing pr`, false}, {`missing eq null`, true}, {`active eq true`, true}, {`age gt 11 and age le 12`, true}, {`age ge 12 and age lt 13`, true}, {`not (active eq false)`, true}, {`active eq false or age eq 12 and userName eq "Alice"`, true}, {`emails[type eq "work" and value co "example"]`, true}, {`emails[type eq "work" and value co "home"]`, false}, {`emails.value ew "home.com"`, true}, {`userName eq "Al\u0069ce"`, true}, {`emails[type eq "work" or (type eq "home" and value pr)]`, true},
	} {
		t.Run(tc.q, func(t *testing.T) {
			f, e := ParseFilter(tc.q)
			if e != nil {
				t.Fatal(e)
			}
			if got := f.Match(r); got != tc.want {
				t.Fatalf("got %v want %v", got, tc.want)
			}
		})
	}
}
func TestInvalidFilters(t *testing.T) {
	for _, q := range []string{"", `userName eq`, `userName eq 'alice'`, `userName eq "x" garbage`, `1name eq "x"`, `name..givenName pr`, `active gt true`, `age eq 01`, `a eq "\x"`, `emails[]`, `emails[type eq "work"`, `not active eq true`, `a eq true AND`, `emails[emails[type pr]]`} {
		t.Run(q, func(t *testing.T) {
			_, e := ParseFilter(q)
			var se *Error
			if !errors.As(e, &se) || se.ScimType != "invalidFilter" || se.Status != 400 {
				t.Fatalf("expected invalidFilter, got %v", e)
			}
		})
	}
}
func TestOpaqueStringNotDateTime(t *testing.T) {
	for _, attr := range []string{"id", "userName"} {
		f, e := ParseFilter(attr + ` eq "2024-01-01T00:00:00Z"`)
		if e != nil {
			t.Fatal(e)
		}
		if f.Match(map[string]any{attr: "2023-12-31T19:00:00-05:00"}) {
			t.Fatal("opaque string compared as dateTime")
		}
	}
}
func TestCaseExactMemberReferences(t *testing.T) {
	r := map[string]any{"members": []any{map[string]any{"value": "ABC"}}}
	for _, q := range []string{`members[value eq "abc"]`, `members.value eq "abc"`} {
		f, e := ParseFilter(q)
		if e != nil {
			t.Fatal(e)
		}
		if f.Match(r) {
			t.Fatalf("reference matched with wrong case: %s", q)
		}
	}
	got, e := ApplyPatch(r, []Operation{{Op: "remove", Path: `members[value eq "abc"]`}})
	if e != nil || len(got["members"].([]any)) != 1 {
		t.Fatalf("wrong member removed: %v %v", got, e)
	}
}
func TestFilterOperatorBoundaries(t *testing.T) {
	for _, s := range []string{`a eq|ne "x"`, `a | "x"`} {
		if _, e := ParseFilter(s); e == nil {
			t.Fatalf("accepted %s", s)
		}
	}
	f, e := ParseFilter(`userName co ""`)
	if e != nil || !f.Match(map[string]any{"userName": "Alice"}) {
		t.Fatalf("empty substring: %v", e)
	}
}
func TestFilterBoundaries(t *testing.T) {
	for _, tc := range []struct {
		q    string
		r    map[string]any
		want bool
	}{
		{`id eq "ABC"`, map[string]any{"id": "abc"}, false},
		{`nickname eq null`, map[string]any{"nickname": ""}, true},
		{`emails eq null`, map[string]any{"emails": []any{}}, true},
		{`meta.created gt "2024-01-01T00:00:00Z"`, map[string]any{"meta": map[string]any{"created": "2023-12-31T20:00:01-04:00"}}, true},
		{`age eq 9007199254740993`, map[string]any{"age": json.Number("9007199254740993")}, true},
		{`age ne 9007199254740992`, map[string]any{"age": json.Number("9007199254740993")}, true},
		{`urn:ietf:params:scim:schemas:extension:enterprise:2.0:User:department eq "IT"`, map[string]any{"urn:ietf:params:scim:schemas:extension:enterprise:2.0:User": map[string]any{"Department": "IT"}}, true},
	} {
		f, e := ParseFilter(tc.q)
		if e != nil {
			t.Fatal(e)
		}
		if f.Match(tc.r) != tc.want {
			t.Fatalf("%s", tc.q)
		}
	}
	for _, s := range []string{`emails [type pr]`, `x eq 1e999999999`, strings.Repeat("(", 130) + `x pr` + strings.Repeat(")", 130)} {
		if _, e := ParseFilter(s); e == nil {
			t.Fatalf("accepted %q", s)
		}
	}
}
func TestPagination(t *testing.T) {
	for _, tc := range []struct {
		q    string
		s, c int
		bad  bool
	}{
		{"", 1, 100, false}, {"startIndex=0&count=0", 1, 0, false}, {"startIndex=-3&count=-4", 1, 0, false}, {"startIndex=9000&count=1000000", 9000, 1000000, false}, {"startIndex=x", 0, 0, true}, {"count=1.2", 0, 0, true}, {"count=999999999999999999999999999999", 0, 0, true},
	} {
		v, _ := url.ParseQuery(tc.q)
		s, c, e := ParsePagination(v, 100)
		if tc.bad {
			if e == nil {
				t.Fatal(tc.q)
			}
			continue
		}
		if e != nil || s != tc.s || c != tc.c {
			t.Fatalf("%s: %d %d %v", tc.q, s, c, e)
		}
	}
}
func FuzzParseFilter(f *testing.F) {
	for _, s := range []string{`userName eq "Alice"`, `emails[type eq "work"]`, `a eq "\\\""`, ""} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		filter, e := ParseFilter(s)
		if e == nil {
			filter.Match(patchResource())
		}
	})
}

func TestErrorEnvelope(t *testing.T) {
	_, err := ParseFilter("")
	b, e := json.Marshal(err)
	if e != nil {
		t.Fatal(e)
	}
	var v map[string]any
	if e = json.Unmarshal(b, &v); e != nil {
		t.Fatal(e)
	}
	if v["status"] != "400" || v["scimType"] != "invalidFilter" || v["schemas"] == nil {
		t.Fatalf("%s", b)
	}
}
