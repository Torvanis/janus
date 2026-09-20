package httpapi

import (
	"context"
	"encoding/json"
	"github.com/go-chi/chi/v5"
	"github.com/torvanis/janus/internal/store"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func scimRequest(t *testing.T, h http.Handler, method, path, secret, body string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Host = "attacker.invalid"
	r.Header.Set("Content-Type", "application/scim+json")
	if secret != "" {
		r.Header.Set("Authorization", "Bearer "+secret)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	out := map[string]any{}
	if w.Body.Len() > 0 {
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatalf("non-JSON %d: %s", w.Code, w.Body.String())
		}
	}
	return w, out
}
func TestSCIMHTTPResources(t *testing.T) {
	h := newHarness(t)
	r := chi.NewRouter()
	h.server.mountSCIMRoutes(r)
	_, secret, err := h.store.CreateSCIMToken(context.Background(), "test", time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	body := `{"schemas":["urn:ietf:params:scim:schemas:core:2.0:User"],"userName":"scim@example.com","active":true,"id":"forged","meta":{"location":"https://attacker.invalid"}}`
	w, b := scimRequest(t, r, "POST", "/scim/v2/Users", secret, body)
	if w.Code != 201 {
		t.Fatalf("create: %d %v", w.Code, b)
	}
	id := b["id"].(string)
	if id == "forged" || w.Header().Get("Location") != "http://janus.test/scim/v2/Users/"+id {
		t.Fatal(w.Header(), b)
	}
	w, b = scimRequest(t, r, "GET", "/scim/v2/Users?count=0", secret, "")
	if w.Code != 200 || b["totalResults"] != float64(1) || len(b["Resources"].([]any)) != 0 {
		t.Fatal(w.Code, b)
	}
	w, b = scimRequest(t, r, "GET", "/scim/v2/Users?filter=userName%20eq%20%22scim@example.com%22", secret, "")
	if w.Code != 200 || b["totalResults"] != float64(1) {
		t.Fatal(w.Code, b)
	}
	w, b = scimRequest(t, r, "POST", "/scim/v2/Users", secret, body)
	if w.Code != 409 || b["scimType"] != "uniqueness" {
		t.Fatal(w.Code, b)
	}
	w, b = scimRequest(t, r, "PATCH", "/scim/v2/Users/"+id, secret, `{"schemas":["urn:ietf:params:scim:api:messages:2.0:PatchOp"],"Operations":[{"op":"replace","path":"active","value":false}]}`)
	if w.Code != 200 || b["active"] != false {
		t.Fatal(w.Code, b)
	}
	w, b = scimRequest(t, r, "POST", "/scim/v2/Groups", secret, `{"schemas":["urn:ietf:params:scim:schemas:core:2.0:Group"],"displayName":"Directory team","members":[{"value":"`+id+`"}]}`)
	if w.Code != 201 {
		t.Fatal(w.Code, b)
	}
	gid := b["id"].(string)
	w, b = scimRequest(t, r, "PUT", "/scim/v2/Groups/"+gid, secret, `{"schemas":["urn:ietf:params:scim:schemas:core:2.0:Group"],"displayName":"Renamed team","members":[]}`)
	if w.Code != 200 || b["displayName"] != "Renamed team" {
		t.Fatal(w.Code, b)
	}
	for _, path := range []string{"/scim/v2/Users/" + id, "/scim/v2/Groups/" + gid} {
		w, _ = scimRequest(t, r, "DELETE", path, secret, "")
		if w.Code != 204 || w.Body.Len() != 0 {
			t.Fatal(w.Code, w.Body.String())
		}
		w, b = scimRequest(t, r, "GET", path, secret, "")
		if w.Code != 404 {
			t.Fatal(w.Code, b)
		}
	}
}
func TestSCIMHTTPValidation(t *testing.T) {
	h := newHarness(t)
	r := chi.NewRouter()
	h.server.mountSCIMRoutes(r)
	_, secret, err := h.store.CreateSCIMToken(context.Background(), "test", time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{`{`, `null`, `{} {}`, `{"schemas":[]}`, `{"schemas":["unsupported"]}`, `{"schemas":["urn:ietf:params:scim:schemas:core:2.0:User"],"userName":123}`, `{"schemas":["urn:ietf:params:scim:schemas:core:2.0:User"],"userName":"x","password":"secret"}`} {
		w, b := scimRequest(t, r, "POST", "/scim/v2/Users", secret, body)
		if w.Code != 400 {
			t.Fatalf("body %s: %d %v", body, w.Code, b)
		}
	}
	for _, q := range []string{"filter=(", "count=bad", "sortBy=userName", "attributes=userName&excludedAttributes=active"} {
		w, b := scimRequest(t, r, "GET", "/scim/v2/Users?"+q, secret, "")
		if w.Code != 400 {
			t.Fatal(q, w.Code, b)
		}
	}
	w, b := scimRequest(t, r, "POST", "/scim/v2/Users", secret, `{"schemas":["urn:ietf:params:scim:schemas:core:2.0:User"],"userName":"`+strings.Repeat("x", 2<<20)+`"}`)
	if w.Code != 413 {
		t.Fatal(w.Code, b)
	}
	w, b = scimRequest(t, r, "POST", "/scim/v2/Users/.search", secret, `{}`)
	if w.Code != 405 {
		t.Fatal(w.Code, b)
	}
}
func TestSCIMHTTPPatchAndSchemaSafety(t *testing.T) {
	h := newHarness(t)
	r := chi.NewRouter()
	h.server.mountSCIMRoutes(r)
	_, secret, err := h.store.CreateSCIMToken(context.Background(), "test", time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	w, b := scimRequest(t, r, "POST", "/scim/v2/Users", secret, `{"SCHEMAS":["urn:ietf:params:scim:schemas:core:2.0:User"],"USERNAME":"case@example.com","NAME":{"GIVENNAME":"Case"}}`)
	if w.Code != 201 || b["userName"] != "case@example.com" || b["name"].(map[string]any)["givenName"] != "Case" {
		t.Fatal(w.Code, b)
	}
	id := b["id"].(string)
	for _, item := range []struct{ operation, kind string }{
		{`{"op":"replace","path":"unknownAttribute","value":true}`, "invalidPath"},
		{`{"op":"replace","path":"groups","value":[]}`, "mutability"},
		{`{"op":"add","path":"urn:ietf:params:scim:schemas:core:2.0:User:password","value":"secret"}`, "mutability"},
		{`{"op":"add","value":{"Password":"secret"}}`, "mutability"},
		{`{"op":"replace","path":"emails[type eq \"missing\"].value","value":"new@example.com"}`, "noTarget"},
		{`{"op":"replace","path":"emails.value.typo","value":"x"}`, "invalidPath"},
	} {
		w, b = scimRequest(t, r, "PATCH", "/scim/v2/Users/"+id, secret, `{"schemas":["urn:ietf:params:scim:api:messages:2.0:PatchOp"],"Operations":[{"op":"replace","path":"active","value":false},`+item.operation+`]}`)
		if w.Code != 400 || b["scimType"] != item.kind {
			t.Fatal(item.operation, w.Code, b)
		}
		w, b = scimRequest(t, r, "GET", "/scim/v2/Users/"+id, secret, "")
		if w.Code != 200 || b["active"] == false {
			t.Fatal("failed PATCH changed resource", w.Code, b)
		}
	}
	w, b = scimRequest(t, r, "POST", "/scim/v2/Users", secret, `{"schemas":["urn:ietf:params:scim:schemas:core:2.0:User"],"userName":"secret@example.com","Password":"secret"}`)
	if w.Code != 400 || b["scimType"] != "mutability" {
		t.Fatal(w.Code, b)
	}
	w, b = scimRequest(t, r, "POST", "/scim/v2/Users", secret, `{"schemas":["urn:ietf:params:scim:schemas:core:2.0:User"],"userName":"primary@example.com","emails":[{"value":"a@example.com","primary":true},{"value":"b@example.com","primary":true}]}`)
	if w.Code != 400 {
		t.Fatal(w.Code, b)
	}
}
func TestSCIMHTTPProjectionAndLimits(t *testing.T) {
	h := newHarness(t)
	r := chi.NewRouter()
	h.server.mountSCIMRoutes(r)
	_, secret, err := h.store.CreateSCIMToken(context.Background(), "test", time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	w, b := scimRequest(t, r, "POST", "/scim/v2/Users", secret, `{"schemas":["urn:ietf:params:scim:schemas:core:2.0:User","urn:ietf:params:scim:schemas:extension:enterprise:2.0:User"],"userName":"projection@example.com","name":{"givenName":"A","familyName":"B"},"emails":[{"value":"a@example.com","type":"work"}],"urn:ietf:params:scim:schemas:extension:enterprise:2.0:User":{"department":"IT"}}`)
	if w.Code != 201 {
		t.Fatal(w.Code, b)
	}
	id := b["id"].(string)
	created := b["meta"].(map[string]any)["created"]
	w, b = scimRequest(t, r, "GET", "/scim/v2/Users/"+id+"?attributes=urn:ietf:params:scim:schemas:core:2.0:User:userName,name.givenName,emails.value", secret, "")
	if w.Code != 200 || b["userName"] != "projection@example.com" || b["id"] != id || b["name"].(map[string]any)["familyName"] != nil || b["emails"].([]any)[0].(map[string]any)["type"] != nil {
		t.Fatal(w.Code, b)
	}
	w, b = scimRequest(t, r, "GET", "/scim/v2/Users?startIndex=999999999&count=200", secret, "")
	if w.Code != 200 || b["totalResults"] != float64(1) || b["itemsPerPage"] != float64(0) {
		t.Fatal(w.Code, b)
	}
	w, b = scimRequest(t, r, "PATCH", "/scim/v2/Users/"+id, secret, `{"SCHEMAS":["urn:ietf:params:scim:api:messages:2.0:PatchOp"],"operations":[{"OP":"replace","PATH":"active","VALUE":false}]}`)
	if w.Code != 200 || b["active"] != false || b["meta"].(map[string]any)["created"] != created {
		t.Fatal(w.Code, b)
	}
	operations := strings.TrimSuffix(strings.Repeat(`{"op":"replace","path":"active","value":true},`, 101), ",")
	w, b = scimRequest(t, r, "PATCH", "/scim/v2/Users/"+id, secret, `{"schemas":["urn:ietf:params:scim:api:messages:2.0:PatchOp"],"Operations":[`+operations+`]}`)
	if w.Code != 400 {
		t.Fatal(w.Code, b)
	}
}
func TestSCIMHTTPRequiredAndImmutable(t *testing.T) {
	h := newHarness(t)
	r := chi.NewRouter()
	h.server.mountSCIMRoutes(r)
	_, secret, err := h.store.CreateSCIMToken(context.Background(), "test", time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	ids := []string{}
	for _, name := range []string{"first@example.com", "second@example.com"} {
		w, b := scimRequest(t, r, "POST", "/scim/v2/Users", secret, `{"schemas":["urn:ietf:params:scim:schemas:core:2.0:User"],"userName":"`+name+`"}`)
		if w.Code != 201 {
			t.Fatal(w.Code, b)
		}
		ids = append(ids, b["id"].(string))
	}
	w, b := scimRequest(t, r, "POST", "/scim/v2/Groups", secret, `{"schemas":["urn:ietf:params:scim:schemas:core:2.0:Group"],"displayName":"immutable test","members":[{"value":"`+ids[0]+`"}]}`)
	if w.Code != 201 {
		t.Fatal(w.Code, b)
	}
	id := b["id"].(string)
	for _, op := range []string{`{"op":"remove","path":"displayName"}`, `{"op":"replace","path":"members.value","value":"` + ids[1] + `"}`, `{"op":"replace","value":{"members.value":"` + ids[1] + `"}}`} {
		w, b = scimRequest(t, r, "PATCH", "/scim/v2/Groups/"+id, secret, `{"schemas":["urn:ietf:params:scim:api:messages:2.0:PatchOp"],"Operations":[`+op+`]}`)
		if w.Code != 400 || b["scimType"] != "mutability" {
			t.Fatal(w.Code, b)
		}
	}
}
func TestSCIMHTTPAuditAndRotation(t *testing.T) {
	h := newHarness(t)
	r := chi.NewRouter()
	h.server.mountSCIMRoutes(r)
	h.server.mountSCIMAdminRoutes(r)
	token, secret, err := h.store.CreateSCIMToken(context.Background(), "test", time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	w, b := scimRequest(t, r, "POST", "/scim/v2/Users", secret, `{"schemas":["urn:ietf:params:scim:schemas:core:2.0:User"],"userName":"private-audit@example.com"}`)
	if w.Code != 201 {
		t.Fatal(w.Code, b)
	}
	entries, _, err := h.store.ListAudit(context.Background(), store.AuditFilter{})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, entry := range entries {
		raw, _ := json.Marshal(entry)
		if strings.Contains(string(raw), secret) || strings.Contains(string(raw), "private-audit@example.com") {
			t.Fatal("sensitive audit contents")
		}
		if entry.Action == "scim.create" {
			found = true
			if !strings.Contains(entry.ActorLabel, token.ID) {
				t.Fatal("audit lacks provisioning actor", entry)
			}
		}
	}
	if !found {
		t.Fatal("missing resource audit")
	}
	w, b = scimRequest(t, r, "POST", "/admin/scim/tokens/"+token.ID+"/rotate", "", "")
	if w.Code == 501 {
		tokens, err := h.store.ListSCIMTokens(context.Background())
		if err != nil || len(tokens) != 1 {
			t.Fatal("unsupported rotation changed state")
		}
		if _, err = h.store.AuthenticateSCIMToken(context.Background(), secret); err != nil {
			t.Fatal("unsupported rotation revoked credential")
		}
	} else if w.Code == 201 {
		if _, err = h.store.AuthenticateSCIMToken(context.Background(), secret); err == nil {
			t.Fatal("rotation retained original credential")
		}
		if _, err = h.store.AuthenticateSCIMToken(context.Background(), b["secret"].(string)); err != nil {
			t.Fatal(err)
		}
	} else {
		t.Fatal(w.Code, b)
	}
}
func TestSCIMHTTPAdminTokens(t *testing.T) {
	h := newHarness(t)
	withBusinessLicense(t, h)
	r := chi.NewRouter()
	h.server.mountSCIMAdminRoutes(r)
	w, b := scimRequest(t, r, "POST", "/admin/scim/tokens", "", `{"name":"directory","expires_at":"`+time.Now().Add(time.Hour).UTC().Format(time.RFC3339)+`"}`)
	if w.Code != 201 {
		t.Fatal(w.Code, b)
	}
	secret := b["secret"].(string)
	id := b["token"].(map[string]any)["id"].(string)
	if !strings.HasPrefix(secret, "scim_") {
		t.Fatal("incorrect credential kind")
	}
	w, b = scimRequest(t, r, "GET", "/admin/scim/tokens", "", "")
	if w.Code != 200 || strings.Contains(w.Body.String(), secret) || strings.Contains(w.Body.String(), "digest") {
		t.Fatal(w.Code, b)
	}
	for _, body := range []string{`{}`, `{"name":"x","expires_at":"2020-01-01T00:00:00Z"}`, `{"name":"x","expires_at":"garbage"}`, `{"name":"` + strings.Repeat("x", 201) + `"}`} {
		w, b = scimRequest(t, r, "POST", "/admin/scim/tokens", "", body)
		if w.Code != 400 {
			t.Fatal(w.Code, b)
		}
	}
	w, b = scimRequest(t, r, "GET", "/admin/scim/config", "", "")
	if w.Code != 200 || b["base_url"] != "http://janus.test/scim/v2" || b["enabled"] != true {
		t.Fatal(w.Code, b)
	}
	w, b = scimRequest(t, r, "DELETE", "/admin/scim/tokens/"+id, "", "")
	if w.Code != 204 {
		t.Fatal(w.Code, b)
	}
	if _, err := h.store.AuthenticateSCIMToken(context.Background(), secret); err == nil {
		t.Fatal("revoked credential accepted")
	}
}
func TestSCIMHTTPSchemas(t *testing.T) {
	h := newHarness(t)
	r := chi.NewRouter()
	h.server.mountSCIMRoutes(r)
	_, secret, err := h.store.CreateSCIMToken(context.Background(), "test", time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range []struct {
		path  string
		count float64
	}{{"Schemas", 3}, {"ResourceTypes", 2}} {
		w, b := scimRequest(t, r, "GET", "/scim/v2/"+item.path, secret, "")
		if w.Code != 200 || b["totalResults"] != item.count {
			t.Fatal(w.Code, b)
		}
		for _, v := range b["Resources"].([]any) {
			resource := v.(map[string]any)
			id := resource["id"].(string)
			w, schema := scimRequest(t, r, "GET", "/scim/v2/"+item.path+"/"+id, secret, "")
			if w.Code != 200 || schema["id"] != id {
				t.Fatal(w.Code, schema)
			}
			if item.path == "Schemas" && len(schema["attributes"].([]any)) < 2 {
				t.Fatal(schema)
			}
		}
	}
}
func TestSCIMHTTPAuthenticationDiscovery(t *testing.T) {
	h := newHarness(t)
	r := chi.NewRouter()
	h.server.mountSCIMRoutes(r)
	for _, secret := range []string{"", h.token, "svc_invalid"} {
		w, b := scimRequest(t, r, "GET", "/scim/v2/ServiceProviderConfig", secret, "")
		if w.Code != 401 || b["status"] != "401" {
			t.Fatalf("unauthorized: %d %v", w.Code, b)
		}
	}
	tok, secret, err := h.store.CreateSCIMToken(context.Background(), "test", time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	w, b := scimRequest(t, r, "GET", "/scim/v2/ServiceProviderConfig", secret, "")
	if w.Code != 200 || w.Header().Get("Content-Type") != "application/scim+json" {
		t.Fatalf("discovery: %d %v", w.Code, b)
	}
	if b["bulk"].(map[string]any)["supported"] != false || b["sort"].(map[string]any)["supported"] != false {
		t.Fatal(b)
	}
	for _, path := range []string{"/scim/v2/absent", "/scim/v2/Bulk"} {
		w, b = scimRequest(t, r, "GET", path, secret, "")
		if w.Code != 404 || b["status"] != "404" {
			t.Fatal(w.Code, b)
		}
	}
	if err = h.store.RevokeSCIMToken(context.Background(), tok.ID); err != nil {
		t.Fatal(err)
	}
	w, _ = scimRequest(t, r, "GET", "/scim/v2/Schemas", secret, "")
	if w.Code != 401 {
		t.Fatal(w.Code)
	}
}
