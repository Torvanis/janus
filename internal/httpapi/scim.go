package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/torvanis/janus/internal/scim"
	"github.com/torvanis/janus/internal/store"
)

const scimCore = "urn:ietf:params:scim:schemas:core:2.0:"
const scimEnterprise = "urn:ietf:params:scim:schemas:extension:enterprise:2.0:User"
const scimListSchema = "urn:ietf:params:scim:api:messages:2.0:ListResponse"
const scimMaxResults = 200
const scimPatchSchema = "urn:ietf:params:scim:api:messages:2.0:PatchOp"

func scimDecode(w http.ResponseWriter, r *http.Request, v any) bool {
	media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || (media != "application/scim+json" && media != "application/json") {
		scimFailure(w, 415, "", "Expected application/scim+json")
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, 2<<20)
	dec := json.NewDecoder(r.Body)
	if err = dec.Decode(v); err == nil {
		var extra any
		if e := dec.Decode(&extra); e != io.EOF {
			if e == nil {
				err = errors.New("extra JSON")
			} else {
				err = e
			}
		}
	}
	if err != nil {
		var large *http.MaxBytesError
		if errors.As(err, &large) {
			scimFailure(w, 413, "", "Request exceeds 2 MiB")
		} else {
			scimFailure(w, 400, "invalidSyntax", "Expected one JSON document")
		}
		return false
	}
	return true
}
func scimHasSchema(v any, want string) bool {
	arr, ok := v.([]any)
	if !ok || len(arr) == 0 {
		return false
	}
	seen := false
	for _, a := range arr {
		str, ok := a.(string)
		if !ok {
			return false
		}
		if str == want {
			seen = true
		}
	}
	return seen
}
func scimBad(detail string) error {
	return &scim.Error{Status: 400, ScimType: "invalidValue", Detail: detail}
}

// scimNormalizeObject canonicalizes case-insensitive schema attributes, validates
// types, and ignores unrecognized/read-only input attributes (RFC 7643).
func scimNormalizeObject(body map[string]any, attrs []map[string]any) error {
	keys := make([]string, 0, len(body))
	seen := map[string]bool{}
	for key := range body {
		lower := strings.ToLower(key)
		if seen[lower] {
			return scimBad("Duplicate case-insensitive attribute")
		}
		seen[lower] = true
		keys = append(keys, key)
	}
	for _, key := range keys {
		value := body[key]
		if strings.EqualFold(key, "password") {
			return &scim.Error{Status: 400, ScimType: "mutability", Detail: "Password provisioning is not supported"}
		}
		var attr map[string]any
		for _, a := range attrs {
			if strings.EqualFold(key, a["name"].(string)) {
				attr = a
				break
			}
		}
		if attr == nil {
			delete(body, key)
			continue
		}
		canonical := attr["name"].(string)
		if key != canonical {
			delete(body, key)
			body[canonical] = value
		}
		if attr["mutability"] == "readOnly" && canonical != "schemas" {
			delete(body, canonical)
			continue
		}
		if value == nil {
			delete(body, canonical)
			continue
		}
		values := []any{value}
		if attr["multiValued"] == true {
			var ok bool
			values, ok = value.([]any)
			if !ok {
				return scimBad(canonical + " must be an array")
			}
		}
		primary := 0
		for _, v := range values {
			switch attr["type"] {
			case "string", "reference":
				if _, ok := v.(string); !ok {
					return scimBad(canonical + " must contain strings")
				}
			case "boolean":
				if _, ok := v.(bool); !ok {
					return scimBad(canonical + " must be boolean")
				}
			case "complex":
				m, ok := v.(map[string]any)
				if !ok {
					return scimBad(canonical + " must contain objects")
				}
				subs, _ := attr["subAttributes"].([]map[string]any)
				if err := scimNormalizeObject(m, subs); err != nil {
					return err
				}
				if m["primary"] == true {
					primary++
				}
			}
		}
		if primary > 1 {
			return scimBad("Only one primary value is permitted per attribute")
		}
	}
	return nil
}
func scimResourceAttributes(kind string) []map[string]any {
	schemas := scimProfileSchemas()
	i := 0
	if kind == "Groups" {
		i = 1
	}
	attrs := append([]map[string]any{}, schemas[i]["attributes"].([]map[string]any)...)
	attrs = append(attrs, scimAttribute("schemas", "string", true, true, "readOnly", "always", "none"), scimAttribute("id", "string", false, false, "readOnly", "always", "global"), scimAttribute("meta", "complex", false, false, "readOnly", "default", "none"))
	if kind == "Users" {
		attrs = append(attrs, scimComplex(scimEnterprise, false, schemas[2]["attributes"].([]map[string]any)...))
	}
	return attrs
}
func scimValidateResource(kind string, body map[string]any) error {
	if err := scimNormalizeObject(body, scimResourceAttributes(kind)); err != nil {
		return err
	}
	schema := scimCore + strings.TrimSuffix(kind, "s")
	if !scimHasSchema(body["schemas"], schema) {
		return scimBad("Required core schema is missing")
	}
	seen := map[string]bool{}
	for _, v := range body["schemas"].([]any) {
		name := v.(string)
		if seen[name] {
			return scimBad("Duplicate schema")
		}
		seen[name] = true
		if name != schema && (kind != "Users" || name != scimEnterprise) {
			return scimBad("Unsupported resource schema")
		}
	}
	if _, ok := body[scimEnterprise]; ok && !seen[scimEnterprise] {
		return scimBad("Enterprise extension must be declared in schemas")
	}
	return nil
}
func scimPatchAttribute(kind, path string) (map[string]any, error) {
	bad := func() (map[string]any, error) {
		return nil, &scim.Error{Status: 400, ScimType: "invalidPath", Detail: "Unknown or unsupported attribute path"}
	}
	attrs := scimResourceAttributes(kind)
	core := scimCore + strings.TrimSuffix(kind, "s") + ":"
	if len(path) > len(core) && strings.EqualFold(path[:len(core)], core) {
		path = path[len(core):]
	}
	// Filter syntax is validated by ApplyPatch; remove it only for schema lookup.
	if open := strings.IndexByte(path, '['); open >= 0 {
		close := strings.LastIndexByte(path, ']')
		if close < open {
			return bad()
		}
		path = path[:open] + path[close+1:]
	}
	if len(path) > len(scimEnterprise) && strings.EqualFold(path[:len(scimEnterprise)], scimEnterprise) && path[len(scimEnterprise)] == ':' && kind == "Users" {
		attrs = scimProfileSchemas()[2]["attributes"].([]map[string]any)
		path = path[len(scimEnterprise)+1:]
	}
	if strings.EqualFold(path, scimEnterprise) && kind == "Users" {
		return scimComplex(scimEnterprise, false, scimProfileSchemas()[2]["attributes"].([]map[string]any)...), nil
	}
	var found map[string]any
	for _, part := range strings.Split(path, ".") {
		if strings.EqualFold(part, "password") {
			return nil, &scim.Error{Status: 400, ScimType: "mutability", Detail: "Password provisioning is not supported"}
		}
		found = nil
		for _, a := range attrs {
			if strings.EqualFold(part, a["name"].(string)) {
				found = a
				break
			}
		}
		if found == nil {
			return bad()
		}
		if found["mutability"] == "readOnly" {
			return nil, &scim.Error{Status: 400, ScimType: "mutability", Detail: "Attribute is read-only"}
		}
		attrs, _ = found["subAttributes"].([]map[string]any)
	}
	return found, nil
}
func scimCheckPatch(kind string, op scim.Operation) error {
	if op.Path == "" {
		if m, ok := op.Value.(map[string]any); ok {
			for path, value := range m {
				if err := scimCheckPatch(kind, scim.Operation{Op: op.Op, Path: path, Value: value}); err != nil {
					return err
				}
			}
		}
		return nil
	}
	attr, err := scimPatchAttribute(kind, op.Path)
	if err != nil {
		return err
	}
	if attr["required"] == true && (strings.EqualFold(op.Op, "remove") || op.Value == nil) {
		return &scim.Error{Status: 400, ScimType: "mutability", Detail: "Required attributes cannot be removed"}
	}
	if attr["type"] == "complex" {
		values := []any{op.Value}
		if list, ok := op.Value.([]any); ok {
			values = list
		}
		for _, v := range values {
			if m, ok := v.(map[string]any); ok {
				for key, value := range m {
					sep := "."
					if strings.EqualFold(op.Path, scimEnterprise) {
						sep = ":"
					}
					if err := scimCheckPatch(kind, scim.Operation{Op: op.Op, Path: op.Path + sep + key, Value: value}); err != nil {
						return err
					}
				}
			}
		}
	}
	return nil
}

// A member's identity is immutable; replacing the members collection is
// allowed, but rewriting an existing record's value/type/reference is not.
func scimCheckImmutable(body map[string]any, kind string, op scim.Operation) error {
	if op.Path == "" {
		if m, ok := op.Value.(map[string]any); ok {
			for path, value := range m {
				if err := scimCheckImmutable(body, kind, scim.Operation{Op: op.Op, Path: path, Value: value}); err != nil {
					return err
				}
			}
		}
		return nil
	}
	attr, err := scimPatchAttribute(kind, op.Path)
	if err != nil {
		return err
	}
	if attr["mutability"] != "immutable" {
		return nil
	}
	path := op.Path
	prefix := scimCore + "Group:"
	if len(path) > len(prefix) && strings.EqualFold(path[:len(prefix)], prefix) {
		path = path[len(prefix):]
	}
	dot := strings.LastIndexByte(path, '.')
	if dot < 0 {
		return nil
	}
	parent, field := path[:dot], path[dot+1:]
	var filter *scim.Filter
	if strings.Contains(parent, "[") {
		filter, err = scim.ParseFilter(parent)
		if err != nil {
			return &scim.Error{Status: 400, ScimType: "invalidPath", Detail: "Invalid member selector"}
		}
	}
	members, _ := body["members"].([]any)
	for _, v := range members {
		member, ok := v.(map[string]any)
		if !ok {
			continue
		}
		if filter != nil && !filter.Match(map[string]any{"members": []any{member}}) {
			continue
		}
		for key, old := range member {
			if strings.EqualFold(key, field) && old != nil && old != "" && (strings.EqualFold(op.Op, "remove") || old != op.Value) {
				return &scim.Error{Status: 400, ScimType: "mutability", Detail: "Member identity attributes are immutable"}
			}
		}
	}
	return nil
}
func (s *Server) scimPresent(kind string, body map[string]any) map[string]any {
	delete(body, "password")
	schema := scimCore + strings.TrimSuffix(kind, "s")
	schemas := []any{schema}
	if _, ok := body[scimEnterprise]; ok && kind == "Users" {
		schemas = append(schemas, scimEnterprise)
	}
	body["schemas"] = schemas
	meta, _ := body["meta"].(map[string]any)
	if meta == nil {
		meta = map[string]any{}
	}
	meta["resourceType"] = strings.TrimSuffix(kind, "s")
	meta["location"] = s.scimBase() + "/" + kind + "/" + url.PathEscape(fmt.Sprint(body["id"]))
	delete(meta, "version")
	body["meta"] = meta
	return body
}
func scimQuery(r *http.Request) (int, int, *scim.Filter, error) {
	q := r.URL.Query()
	for _, k := range []string{"sortBy", "sortOrder"} {
		if q.Has(k) {
			return 0, 0, nil, scimBad("Sorting is not supported")
		}
	}
	if q.Has("attributes") && q.Has("excludedAttributes") {
		return 0, 0, nil, scimBad("attributes and excludedAttributes are mutually exclusive")
	}
	start, count, err := scim.ParsePagination(q, 100)
	if err != nil {
		return 0, 0, nil, err
	}
	if count > scimMaxResults {
		count = scimMaxResults
	}
	var f *scim.Filter
	if q.Has("filter") {
		if len(q["filter"]) != 1 {
			return 0, 0, nil, scimBad("Only one filter is permitted")
		}
		f, err = scim.ParseFilter(q.Get("filter"))
	}
	return start, count, f, err
}
func scimList(w http.ResponseWriter, resources []map[string]any, start, count int) {
	total := len(resources)
	offset := min(start-1, total)
	end := offset + min(count, total-offset)
	scimJSON(w, 200, map[string]any{"schemas": []string{scimListSchema}, "totalResults": total, "startIndex": start, "itemsPerPage": end - offset, "Resources": resources[offset:end]})
}
func (s *Server) scimResources(w http.ResponseWriter, r *http.Request, kind, id string) {
	start, count, filter, err := scimQuery(r)
	if err != nil {
		scimStoreFailure(w, err)
		return
	}
	if r.Header.Get("If-Match") != "" || r.Header.Get("If-None-Match") != "" {
		scimFailure(w, 400, "invalidValue", "Conditional requests are not supported")
		return
	}
	if r.Method == "GET" && id == "" {
		resources, err := s.Store.ListSCIMResources(r.Context(), kind)
		if err != nil {
			scimStoreFailure(w, err)
			return
		}
		out := []map[string]any{}
		for _, resource := range resources {
			resource = s.scimPresent(kind, resource)
			if filter == nil || filter.Match(resource) {
				out = append(out, scimProject(resource, r.URL.Query()))
			}
		}
		sort.SliceStable(out, func(i, j int) bool { return fmt.Sprint(out[i]["id"]) < fmt.Sprint(out[j]["id"]) })
		scimList(w, out, start, count)
		return
	}
	if r.Method == "GET" {
		resource, err := s.Store.SCIMResource(r.Context(), kind, id)
		if err != nil {
			scimStoreFailure(w, err)
			return
		}
		scimJSON(w, 200, scimProject(s.scimPresent(kind, resource), r.URL.Query()))
		return
	}
	if r.Method == "DELETE" {
		if err := s.Store.DeleteSCIMResource(r.Context(), kind, id); err != nil {
			scimStoreFailure(w, err)
			return
		}
		s.scimAudit(r, "scim.delete", kind, id)
		scimJSON(w, 204, nil)
		return
	}
	var body map[string]any
	if !scimDecode(w, r, &body) {
		return
	}
	if body == nil {
		scimFailure(w, 400, "invalidSyntax", "Expected a JSON object")
		return
	}
	if r.Method == "PATCH" {
		// Envelope names, like resource attributes, are case-insensitive.
		normalized := map[string]any{}
		for key, value := range body {
			k := key
			if strings.EqualFold(key, "schemas") {
				k = "schemas"
			}
			if strings.EqualFold(key, "Operations") {
				k = "Operations"
			}
			if _, ok := normalized[k]; ok {
				scimFailure(w, 400, "invalidSyntax", "Duplicate patch envelope field")
				return
			}
			normalized[k] = value
		}
		body = normalized
		if !scimHasSchema(body["schemas"], scimPatchSchema) || len(body["schemas"].([]any)) != 1 {
			scimFailure(w, 400, "invalidSyntax", "PatchOp schema is required")
			return
		}
		raw, ok := body["Operations"].([]any)
		if !ok || len(raw) == 0 || len(raw) > 100 {
			scimFailure(w, 400, "invalidSyntax", "Operations must contain between 1 and 100 operations")
			return
		}
		operations := make([]scim.Operation, 0, len(raw))
		for _, v := range raw {
			rawJSON, err := json.Marshal(v)
			if err != nil {
				scimFailure(w, 400, "invalidSyntax", "Invalid operation")
				return
			}
			var operation scim.Operation
			if err = json.Unmarshal(rawJSON, &operation); err != nil {
				scimStoreFailure(w, err)
				return
			}
			if err = scimCheckPatch(kind, operation); err != nil {
				scimStoreFailure(w, err)
				return
			}
			operations = append(operations, operation)
		}
		old, err := s.Store.SCIMResource(r.Context(), kind, id)
		if err != nil {
			scimStoreFailure(w, err)
			return
		}
		body = old
		for _, operation := range operations {
			if err = scimCheckImmutable(body, kind, operation); err != nil {
				scimStoreFailure(w, err)
				return
			}
			body, err = scim.ApplyPatch(body, []scim.Operation{operation})
			if err != nil {
				scimStoreFailure(w, err)
				return
			}
		}
	}
	if err := scimValidateResource(kind, body); err != nil {
		scimStoreFailure(w, err)
		return
	}
	delete(body, "id")
	delete(body, "meta")
	delete(body, "groups")
	resource, err := s.Store.SaveSCIMResource(r.Context(), kind, id, body)
	if err != nil {
		scimStoreFailure(w, err)
		return
	}
	resource = s.scimPresent(kind, resource)
	status := 200
	action := "scim.update"
	if r.Method == "POST" {
		status = 201
		action = "scim.create"
	}
	s.scimAudit(r, action, kind, fmt.Sprint(resource["id"]))
	w.Header().Set("Location", resource["meta"].(map[string]any)["location"].(string))
	scimJSON(w, status, scimProject(resource, r.URL.Query()))
}

// Attribute projection retains always-returned identifiers. Paths are case-insensitive.
func scimProject(resource map[string]any, q url.Values) map[string]any {
	include, exclude := q.Get("attributes"), q.Get("excludedAttributes")
	if include == "" && exclude == "" {
		return resource
	}
	out := map[string]any{}
	if include == "" {
		for k, v := range resource {
			out[k] = v
		}
	} else {
		for _, path := range strings.Split(include, ",") {
			scimCopyPath(out, resource, strings.TrimSpace(path))
		}
	}
	for _, path := range strings.Split(exclude, ",") {
		scimDropPath(out, strings.TrimSpace(path))
	}
	for _, key := range []string{"schemas", "id"} {
		if v, ok := resource[key]; ok {
			out[key] = v
		}
	}
	return out
}
func scimSplitPath(resource map[string]any, path string) (string, string) {
	for _, kind := range []string{"User", "Group"} {
		prefix := scimCore + kind + ":"
		if len(path) > len(prefix) && strings.EqualFold(path[:len(prefix)], prefix) {
			path = path[len(prefix):]
			break
		}
	}
	for key := range resource {
		if strings.EqualFold(path, key) {
			return key, ""
		}
		if len(path) > len(key) && strings.EqualFold(path[:len(key)], key) && (path[len(key)] == '.' || path[len(key)] == ':') {
			return key, path[len(key)+1:]
		}
	}
	return "", ""
}
func scimCopyPath(dst, src map[string]any, path string) {
	key, rest := scimSplitPath(src, path)
	if key == "" {
		return
	}
	if rest == "" {
		dst[key] = src[key]
		return
	}
	switch value := src[key].(type) {
	case map[string]any:
		child, _ := dst[key].(map[string]any)
		if child == nil {
			child = map[string]any{}
		}
		scimCopyPath(child, value, rest)
		dst[key] = child
	case []any:
		items, _ := dst[key].([]any)
		if len(items) != len(value) {
			items = make([]any, len(value))
		}
		for i, v := range value {
			m, ok := v.(map[string]any)
			if ok {
				child, _ := items[i].(map[string]any)
				if child == nil {
					child = map[string]any{}
				}
				scimCopyPath(child, m, rest)
				items[i] = child
			}
		}
		dst[key] = items
	}
}
func scimDropPath(dst map[string]any, path string) {
	key, rest := scimSplitPath(dst, path)
	if key == "" {
		return
	}
	if rest == "" {
		delete(dst, key)
		return
	}
	switch value := dst[key].(type) {
	case map[string]any:
		child := map[string]any{}
		for k, v := range value {
			child[k] = v
		}
		scimDropPath(child, rest)
		dst[key] = child
	case []any:
		items := make([]any, len(value))
		for i, v := range value {
			items[i] = v
			if m, ok := v.(map[string]any); ok {
				child := map[string]any{}
				for k, v := range m {
					child[k] = v
				}
				scimDropPath(child, rest)
				items[i] = child
			}
		}
		dst[key] = items
	}
}

// mountSCIMAdminRoutes must be mounted under the application's RequireAdmin and CSRF guards.
func (s *Server) mountSCIMAdminRoutes(r chi.Router) {
	r.Get("/admin/scim/config", func(w http.ResponseWriter, r *http.Request) {
		scimAdminJSON(w, 200, map[string]any{"base_url": s.scimBase(), "documentation": s.scimBase() + "/ServiceProviderConfig", "enabled": true, "group_mapping": map[string]any{"source": "SCIM Groups", "target": "teams", "membership": "Provisioned membership is reconciled independently of manual membership"}})
	})
	r.Get("/admin/scim/tokens", func(w http.ResponseWriter, r *http.Request) {
		tokens, err := s.Store.ListSCIMTokens(r.Context())
		if err != nil {
			scimStoreFailure(w, err)
			return
		}
		scimAdminJSON(w, 200, map[string]any{"tokens": tokens})
	})
	r.Post("/admin/scim/tokens", s.gateFeature("scim", s.gateCreate("SCIM token", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Name      string     `json:"name"`
			ExpiresAt *time.Time `json:"expires_at"`
		}
		if !scimDecode(w, r, &in) {
			return
		}
		in.Name = strings.TrimSpace(in.Name)
		if in.Name == "" || len(in.Name) > 200 {
			scimFailure(w, 400, "invalidValue", "Name must contain 1 to 200 bytes")
			return
		}
		var expires time.Time
		if in.ExpiresAt != nil {
			expires = *in.ExpiresAt
			if !expires.After(time.Now()) {
				scimFailure(w, 400, "invalidValue", "expires_at must be in the future")
				return
			}
		}
		token, secret, err := s.Store.CreateSCIMToken(r.Context(), in.Name, expires)
		if err != nil {
			scimStoreFailure(w, err)
			return
		}
		s.audit(r, "scim.token.create", "scim_token", token.ID, nil, nil)
		scimAdminJSON(w, 201, map[string]any{"token": token, "secret": secret})
	})))
	r.Delete("/admin/scim/tokens/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		if err := s.Store.RevokeSCIMToken(r.Context(), id); err != nil {
			scimStoreFailure(w, err)
			return
		}
		s.audit(r, "scim.token.revoke", "scim_token", id, nil, nil)
		scimAdminJSON(w, 204, nil)
	})
	r.Post("/admin/scim/tokens/{id}/rotate", func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		token, secret, err := s.Store.RotateSCIMToken(r.Context(), id)
		if err != nil {
			scimStoreFailure(w, err)
			return
		}
		s.audit(r, "scim.token.rotate", "scim_token", id, nil, map[string]string{"replacement_id": token.ID})
		scimAdminJSON(w, 201, map[string]any{"token": token, "secret": secret})
	})
}
func scimAdminJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if v != nil {
		_ = json.NewEncoder(w).Encode(v)
	}
}

// Schema documents describe persisted profile attributes; entitlements and roles
// are directory profile data only, never application authorization grants.
func scimAttribute(name, typ string, multi, required bool, mutability, returned, unique string) map[string]any {
	return map[string]any{"name": name, "type": typ, "multiValued": multi, "description": name, "required": required, "caseExact": false, "mutability": mutability, "returned": returned, "uniqueness": unique}
}
func scimStringAttribute(name string) map[string]any {
	return scimAttribute(name, "string", false, false, "readWrite", "default", "none")
}
func scimComplex(name string, multi bool, subs ...map[string]any) map[string]any {
	a := scimAttribute(name, "complex", multi, false, "readWrite", "default", "none")
	a["subAttributes"] = subs
	return a
}
func scimProfileSchemas() []map[string]any {
	username := scimStringAttribute("userName")
	username["required"] = true
	username["uniqueness"] = "server"
	external := scimStringAttribute("externalId")
	external["caseExact"] = true
	user := []map[string]any{username, external, scimComplex("name", false, scimStringAttribute("formatted"), scimStringAttribute("familyName"), scimStringAttribute("givenName"), scimStringAttribute("middleName"), scimStringAttribute("honorificPrefix"), scimStringAttribute("honorificSuffix"))}
	for _, name := range []string{"displayName", "nickName", "title", "userType", "preferredLanguage", "locale", "timezone"} {
		user = append(user, scimStringAttribute(name))
	}
	profile := scimAttribute("profileUrl", "reference", false, false, "readWrite", "default", "none")
	profile["referenceTypes"] = []string{"external"}
	user = append(user, profile, scimAttribute("active", "boolean", false, false, "readWrite", "default", "none"))
	for _, name := range []string{"emails", "phoneNumbers", "ims", "photos", "entitlements", "roles", "x509Certificates"} {
		value := scimStringAttribute("value")
		if name == "photos" {
			value["type"] = "reference"
			value["referenceTypes"] = []string{"external"}
		}
		user = append(user, scimComplex(name, true, value, scimStringAttribute("display"), scimStringAttribute("type"), scimAttribute("primary", "boolean", false, false, "readWrite", "default", "none")))
	}
	address := []map[string]any{}
	for _, name := range []string{"formatted", "streetAddress", "locality", "region", "postalCode", "country", "type"} {
		address = append(address, scimStringAttribute(name))
	}
	address = append(address, scimAttribute("primary", "boolean", false, false, "readWrite", "default", "none"))
	user = append(user, scimComplex("addresses", true, address...))
	groupRef := scimAttribute("$ref", "reference", false, false, "readOnly", "default", "none")
	groupRef["referenceTypes"] = []string{"Group"}
	groups := scimComplex("groups", true, scimAttribute("value", "string", false, false, "readOnly", "default", "none"), groupRef, scimAttribute("display", "string", false, false, "readOnly", "default", "none"), scimAttribute("type", "string", false, false, "readOnly", "default", "none"))
	groups["mutability"] = "readOnly"
	user = append(user, groups)
	display := scimStringAttribute("displayName")
	display["required"] = true
	memberValue := scimAttribute("value", "string", false, false, "immutable", "default", "none")
	memberValue["caseExact"] = true
	memberRef := scimAttribute("$ref", "reference", false, false, "immutable", "default", "none")
	memberRef["referenceTypes"] = []string{"User"}
	memberType := scimAttribute("type", "string", false, false, "immutable", "default", "none")
	memberType["canonicalValues"] = []string{"User"}
	group := []map[string]any{display, external, scimComplex("members", true, memberValue, memberRef, scimAttribute("display", "string", false, false, "readOnly", "default", "none"), memberType)}
	enterprise := []map[string]any{}
	for _, name := range []string{"employeeNumber", "costCenter", "organization", "division", "department"} {
		enterprise = append(enterprise, scimStringAttribute(name))
	}
	managerValue := scimStringAttribute("value")
	managerValue["caseExact"] = true
	managerRef := scimAttribute("$ref", "reference", false, false, "readWrite", "default", "none")
	managerRef["referenceTypes"] = []string{"User"}
	enterprise = append(enterprise, scimComplex("manager", false, managerValue, managerRef, scimAttribute("displayName", "string", false, false, "readOnly", "default", "none")))
	out := []map[string]any{}
	for _, schema := range []struct {
		id, name, description string
		attrs                 []map[string]any
	}{{scimCore + "User", "User", "User profile. Password changes are not supported. Roles and entitlements do not grant application privileges.", user}, {scimCore + "Group", "Group", "Directory groups provision teams. Only direct User members are supported; nested groups are not supported.", group}, {scimEnterprise, "EnterpriseUser", "Enterprise user profile extension.", enterprise}} {
		out = append(out, map[string]any{"schemas": []string{scimCore + "Schema"}, "id": schema.id, "name": schema.name, "description": schema.description, "attributes": schema.attrs})
	}
	return out
}
func (s *Server) scimDiscovery(w http.ResponseWriter, r *http.Request, kind, id string) {
	start, count, filter, err := scimQuery(r)
	if err != nil {
		scimStoreFailure(w, err)
		return
	}
	resources := []map[string]any{}
	if kind == "Schemas" {
		resources = scimProfileSchemas()
	} else {
		for _, name := range []string{"User", "Group"} {
			resource := map[string]any{"schemas": []string{scimCore + "ResourceType"}, "id": name, "name": name, "endpoint": "/" + name + "s", "description": name + " provisioning", "schema": scimCore + name}
			if name == "User" {
				resource["schemaExtensions"] = []any{map[string]any{"schema": scimEnterprise, "required": false}}
			}
			resources = append(resources, resource)
		}
	}
	out := []map[string]any{}
	for _, resource := range resources {
		resource["meta"] = map[string]any{"resourceType": strings.TrimSuffix(kind, "s"), "location": s.scimBase() + "/" + kind + "/" + url.PathEscape(resource["id"].(string))}
		if id != "" && resource["id"] == id {
			scimJSON(w, 200, scimProject(resource, r.URL.Query()))
			return
		}
		if filter == nil || filter.Match(resource) {
			out = append(out, scimProject(resource, r.URL.Query()))
		}
	}
	if id != "" {
		scimFailure(w, 404, "", "Resource not found")
		return
	}
	scimList(w, out, start, count)
}

func scimJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/scim+json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if v != nil {
		_ = json.NewEncoder(w).Encode(v)
	}
}
func scimFailure(w http.ResponseWriter, status int, kind, detail string) {
	scimJSON(w, status, &scim.Error{Status: status, ScimType: kind, Detail: detail})
}
func scimStoreFailure(w http.ResponseWriter, err error) {
	var pe *scim.Error
	var se *store.SCIMStoreError
	switch {
	case errors.As(err, &pe):
		scimJSON(w, pe.Status, pe)
	case errors.As(err, &se):
		scimFailure(w, se.Status, se.SCIMType, se.Detail)
	case errors.Is(err, store.ErrNotFound):
		scimFailure(w, 404, "", "Resource not found")
	default:
		scimFailure(w, 500, "", "Internal server error")
	}
}
func (s *Server) scimBase() string { return strings.TrimRight(s.Config.PublicURL, "/") + "/scim/v2" }

type scimActorKey struct{}

func (s *Server) scimAudit(r *http.Request, action, kind, id string) {
	actor, _ := r.Context().Value(scimActorKey{}).(string)
	entry := &store.AuditEntry{Action: action, ResourceType: kind, ResourceID: id, ActorLabel: "SCIM token " + actor}
	if err := s.Store.AppendAudit(r.Context(), entry); err != nil && s.Logger != nil {
		s.Logger.ErrorContext(r.Context(), "append SCIM audit entry", "action", action)
	}
}
func (s *Server) mountSCIMRoutes(r chi.Router) {
	r.Route("/scim/v2", func(r chi.Router) {
		r.Use(func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				parts := strings.Fields(r.Header.Get("Authorization"))
				if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") || !strings.HasPrefix(parts[1], "scim_") {
					w.Header().Set("WWW-Authenticate", `Bearer realm="scim"`)
					scimFailure(w, 401, "", "Provisioning bearer token required")
					return
				}
				token, err := s.Store.AuthenticateSCIMToken(r.Context(), parts[1])
				if err != nil {
					if !errors.Is(err, store.ErrNotFound) {
						scimFailure(w, 503, "", "Credential verification unavailable")
						return
					}
					w.Header().Set("WWW-Authenticate", `Bearer realm="scim"`)
					scimFailure(w, 401, "", "Invalid provisioning token")
					return
				}
				next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), scimActorKey{}, token.ID)))
			})
		})
		r.NotFound(func(w http.ResponseWriter, r *http.Request) { scimFailure(w, 404, "", "Resource not found") })
		r.MethodNotAllowed(func(w http.ResponseWriter, r *http.Request) { scimFailure(w, 405, "", "Method not supported") })
		r.Get("/ServiceProviderConfig", s.scimProviderConfig)
		for _, kind := range []string{"Users", "Groups"} {
			r.Route("/"+kind, func(r chi.Router) {
				r.Get("/", func(w http.ResponseWriter, r *http.Request) { s.scimResources(w, r, kind, "") })
				r.Post("/", func(w http.ResponseWriter, r *http.Request) { s.scimResources(w, r, kind, "") })
				for _, method := range []string{"GET", "PUT", "PATCH", "DELETE"} {
					r.MethodFunc(method, "/{id}", func(w http.ResponseWriter, r *http.Request) { s.scimResources(w, r, kind, chi.URLParam(r, "id")) })
				}
			})
		}
		for _, kind := range []string{"Schemas", "ResourceTypes"} {
			r.Get("/"+kind, func(w http.ResponseWriter, r *http.Request) { s.scimDiscovery(w, r, kind, "") })
			r.Get("/"+kind+"/{id}", func(w http.ResponseWriter, r *http.Request) { s.scimDiscovery(w, r, kind, chi.URLParam(r, "id")) })
		}
	})
}
func (s *Server) scimProviderConfig(w http.ResponseWriter, r *http.Request) {
	scimJSON(w, 200, map[string]any{"schemas": []string{scimCore + "ServiceProviderConfig"}, "documentationUri": s.scimBase() + "/ServiceProviderConfig", "patch": map[string]any{"supported": true}, "bulk": map[string]any{"supported": false, "maxOperations": 0, "maxPayloadSize": 0}, "filter": map[string]any{"supported": true, "maxResults": scimMaxResults}, "changePassword": map[string]any{"supported": false}, "sort": map[string]any{"supported": false}, "etag": map[string]any{"supported": false}, "authenticationSchemes": []any{map[string]any{"type": "oauthbearertoken", "name": "Provisioning bearer token", "description": "Dedicated administrator-issued SCIM token; application tokens and sessions are not accepted", "primary": true}}})
}
