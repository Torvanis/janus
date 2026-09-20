package scim

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"
)

func patchResource() map[string]any {
	return map[string]any{"id": "id1", "userName": "Alice", "name": map[string]any{"givenName": "Alice", "familyName": "Smith"}, "members": []any{map[string]any{"value": "a"}, map[string]any{"value": "b"}}, "emails": []any{map[string]any{"type": "work", "value": "old"}, map[string]any{"type": "home", "value": "home"}}}
}
func TestPatchOperations(t *testing.T) {
	for _, tc := range []struct {
		name  string
		ops   []Operation
		check func(map[string]any) bool
	}{
		{"add scalar", []Operation{{Op: "AdD", Path: "displayName", Value: "Alice"}}, func(r map[string]any) bool { return r["displayName"] == "Alice" }},
		{"add member", []Operation{{Op: "add", Path: "MEMBERS", Value: []any{map[string]any{"value": "c"}}}}, func(r map[string]any) bool { return len(r["members"].([]any)) == 3 }},
		{"add duplicate", []Operation{{Op: "add", Path: "members", Value: []any{map[string]any{"value": "a"}}}}, func(r map[string]any) bool { return len(r["members"].([]any)) == 2 }},
		{"remove member", []Operation{{Op: "remove", Path: `members[value eq "a"]`}}, func(r map[string]any) bool {
			return len(r["members"].([]any)) == 1 && r["members"].([]any)[0].(map[string]any)["value"] == "b"
		}},
		{"remove absent member", []Operation{{Op: "remove", Path: `members[value eq "none"]`}}, func(r map[string]any) bool { return len(r["members"].([]any)) == 2 }},
		{"replace filtered subattribute", []Operation{{Op: "replace", Path: `emails[type eq "work"].value`, Value: "new"}}, func(r map[string]any) bool {
			return r["emails"].([]any)[0].(map[string]any)["value"] == "new" && r["emails"].([]any)[1].(map[string]any)["value"] == "home"
		}},
		{"replace complex preserves omitted", []Operation{{Op: "replace", Path: "name", Value: map[string]any{"givenName": "Bob"}}}, func(r map[string]any) bool {
			return r["name"].(map[string]any)["givenName"] == "Bob" && r["name"].(map[string]any)["familyName"] == "Smith"
		}},
		{"no path replace", []Operation{{Op: "replace", Value: map[string]any{"UserName": "Bob", "active": false}}}, func(r map[string]any) bool { return r["userName"] == "Bob" && r["active"] == false && r["id"] == "id1" }},
		{"no path add", []Operation{{Op: "add", Value: map[string]any{"displayName": "Bob"}}}, func(r map[string]any) bool { return r["displayName"] == "Bob" }},
		{"remove all", []Operation{{Op: "remove", Path: "members"}}, func(r map[string]any) bool { _, ok := r["members"]; return !ok }},
		{"create subattribute", []Operation{{Op: "replace", Path: "name.middleName", Value: "M"}}, func(r map[string]any) bool { return r["name"].(map[string]any)["middleName"] == "M" }},
		{"remove subattributes", []Operation{{Op: "remove", Path: "emails.value"}}, func(r map[string]any) bool {
			for _, v := range r["emails"].([]any) {
				if _, ok := v.(map[string]any)["value"]; ok {
					return false
				}
			}
			return true
		}},
		{"extension", []Operation{{Op: "add", Path: "urn:ietf:params:scim:schemas:extension:enterprise:2.0:User:department", Value: "IT"}}, func(r map[string]any) bool {
			return r["urn:ietf:params:scim:schemas:extension:enterprise:2.0:User"].(map[string]any)["department"] == "IT"
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := patchResource()
			got, e := ApplyPatch(r, tc.ops)
			if e != nil {
				t.Fatal(e)
			}
			if !tc.check(got) {
				t.Fatalf("unexpected result %#v", got)
			}
			if !reflect.DeepEqual(r, patchResource()) {
				t.Fatal("mutated input")
			}
		})
	}
}
func TestExtensionSchemaAndPrimaryAddition(t *testing.T) {
	schema := "urn:ietf:params:scim:schemas:extension:enterprise:2.0:User"
	r := patchResource()
	r["schemas"] = []any{"urn:ietf:params:scim:schemas:core:2.0:User"}
	got, e := ApplyPatch(r, []Operation{{Op: "add", Path: schema + ":department", Value: "IT"}})
	if e != nil {
		t.Fatal(e)
	}
	if len(got["schemas"].([]any)) != 2 || got["schemas"].([]any)[1] != schema {
		t.Fatalf("missing extension schema: %#v", got["schemas"])
	}
	r["emails"].([]any)[0].(map[string]any)["primary"] = true
	got, e = ApplyPatch(r, []Operation{{Op: "add", Path: "emails", Value: []any{map[string]any{"type": "other", "value": "new", "primary": true}}}})
	if e != nil {
		t.Fatal(e)
	}
	if got["emails"].([]any)[0].(map[string]any)["primary"] != false {
		t.Fatal("old primary not reset")
	}
	if _, e = ApplyPatch(r, []Operation{{Op: "replace", Path: "emails", Value: []any{map[string]any{"primary": true}, map[string]any{"primary": true}}}}); e == nil {
		t.Fatal("multiple incoming primaries accepted")
	}
}
func TestPatchJSONAndPrimary(t *testing.T) {
	for _, s := range []string{`{"op":"add","path":"active"}`, `{"op":"replace","path":null,"value":true}`, `{"op":"remove","path":1}`, `{"value":true}`, `null`} {
		var op Operation
		if e := json.Unmarshal([]byte(s), &op); e == nil {
			t.Fatalf("accepted %s", s)
		}
	}
	var op Operation
	if e := json.Unmarshal([]byte(`{"op":"replace","path":"nickName","value":null}`), &op); e != nil {
		t.Fatal(e)
	}
	r := patchResource()
	a := r["emails"].([]any)
	a[1].(map[string]any)["primary"] = true
	got, e := ApplyPatch(r, []Operation{{Op: "replace", Path: `emails[type eq "work"].primary`, Value: true}})
	if e != nil {
		t.Fatal(e)
	}
	out := got["emails"].([]any)
	if out[0].(map[string]any)["primary"] != true || out[1].(map[string]any)["primary"] != false {
		t.Fatalf("primary not transferred: %#v", out)
	}
}
func TestPatchAdditionalCases(t *testing.T) {
	r := patchResource()
	value := map[string]any{"type": "work", "value": "new"}
	got, e := ApplyPatch(r, []Operation{{Op: "replace", Path: `emails[type eq "work"]`, Value: value}})
	if e != nil {
		t.Fatal(e)
	}
	got["emails"].([]any)[0].(map[string]any)["value"] = "edited"
	if value["value"] != "new" {
		t.Fatal("aliased operation value")
	}
	for _, ops := range [][]Operation{{{Op: "remove", Path: `emails[type pr]`}}, {{Op: "replace", Path: "members", Value: []any{}}}, {{Op: "add", Path: `emails[type eq "work"]`, Value: map[string]any{"display": "Work"}}}, {{Op: "add", Value: map[string]any{"urn:ietf:params:scim:schemas:extension:enterprise:2.0:User": map[string]any{"department": "IT"}}}}} {
		if _, e = ApplyPatch(r, ops); e != nil {
			t.Fatal(e)
		}
	}
	for _, path := range []string{" emails", "emails ", "emails[type pr] .value", "emails[type pr].value.extra", "userName.value", "userName[value pr]"} {
		if _, e = ApplyPatch(r, []Operation{{Op: "replace", Path: path, Value: "x"}}); e == nil {
			t.Fatalf("accepted %s", path)
		}
	}
	if _, e = ApplyPatch(r, nil); e == nil {
		t.Fatal("empty operations accepted")
	}
}
func FuzzPatchPath(f *testing.F) {
	for _, s := range []string{"userName", `emails[type eq "work"].value`, `members[value eq "a"]`, ""} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		for _, op := range []string{"add", "replace", "remove"} {
			r := patchResource()
			ApplyPatch(r, []Operation{{Op: op, Path: s, Value: "x"}})
			if !reflect.DeepEqual(r, patchResource()) {
				t.Fatal("mutated input")
			}
		}
	})
}

func TestPatchErrorsAtomic(t *testing.T) {
	for _, tc := range []struct {
		op   Operation
		kind string
	}{
		{Operation{Op: "remove"}, "noTarget"}, {Operation{Op: "move", Path: "name"}, "invalidSyntax"}, {Operation{Op: "add", Value: "not object"}, "invalidValue"}, {Operation{Op: "replace", Path: "emails[]", Value: "x"}, "invalidPath"}, {Operation{Op: "replace", Path: `emails[type eq "missing"].value`, Value: "x"}, "noTarget"}, {Operation{Op: "replace", Path: "id", Value: "other"}, "mutability"}, {Operation{Op: "remove", Path: "USERname"}, "mutability"}, {Operation{Op: "add", Path: "meta.created", Value: "today"}, "mutability"}, {Operation{Op: "replace", Path: "userName", Value: nil}, "mutability"}, {Operation{Op: "replace", Path: "name.givenName.extra", Value: "x"}, "invalidPath"},
	} {
		t.Run(tc.kind+tc.op.Path, func(t *testing.T) {
			r := patchResource()
			got, e := ApplyPatch(r, []Operation{{Op: "replace", Path: "displayName", Value: "changed"}, tc.op})
			var se *Error
			if !errors.As(e, &se) || se.ScimType != tc.kind {
				t.Fatalf("want %s got %v", tc.kind, e)
			}
			if got != nil || !reflect.DeepEqual(r, patchResource()) {
				t.Fatal("non-atomic failure")
			}
		})
	}
}
