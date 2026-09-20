package scim

import (
	"encoding/json"
	"reflect"
	"sort"
	"strings"
)

// Operation is one RFC 7644 PatchOp operation. Value may be a JSON null.
// UnmarshalJSON distinguishes a missing value from explicit null.
type Operation struct {
	Op    string `json:"op"`
	Path  string `json:"path,omitempty"`
	Value any    `json:"value,omitempty"`
}

// UnmarshalJSON validates operation shape while preserving explicit JSON null.
func (o *Operation) UnmarshalJSON(data []byte) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil || fields == nil {
		return failure("invalidSyntax", "operation must be an object")
	}
	normalized := make(map[string]json.RawMessage, len(fields))
	for k, v := range fields {
		k = strings.ToLower(k)
		if _, exists := normalized[k]; exists {
			return failure("invalidSyntax", "duplicate operation field")
		}
		normalized[k] = v
	}
	var result Operation
	if raw, ok := normalized["op"]; !ok || json.Unmarshal(raw, &result.Op) != nil || result.Op == "" {
		return failure("invalidSyntax", "operation requires an op string")
	}
	if raw, ok := normalized["path"]; ok {
		if string(raw) == "null" || json.Unmarshal(raw, &result.Path) != nil {
			return failure("invalidPath", "path must be a string")
		}
	}
	if raw, ok := normalized["value"]; ok {
		d := json.NewDecoder(strings.NewReader(string(raw)))
		d.UseNumber()
		if d.Decode(&result.Value) != nil {
			return failure("invalidValue", "invalid operation value")
		}
	} else if !strings.EqualFold(result.Op, "remove") {
		return failure("invalidValue", "operation requires value")
	}
	*o = result
	return nil
}
func (o Operation) MarshalJSON() ([]byte, error) {
	m := map[string]any{"op": o.Op}
	if o.Path != "" {
		m["path"] = o.Path
	}
	if !strings.EqualFold(o.Op, "remove") {
		m["value"] = o.Value
	}
	return json.Marshal(m)
}

type patchPath struct {
	attr   attrPath
	filter expression
	sub    string
}

// ApplyPatch returns an independent resource; neither the input nor operation
// values are modified, including when a later operation fails. It enforces
// common id/meta read-only and existing userName required constraints. Schema
// validation (including additional required/immutable attributes and types) is
// the caller's responsibility. Unknown attribute names cannot be rejected
// without a schema registry.
func ApplyPatch(resource map[string]any, operations []Operation) (map[string]any, error) {
	if resource == nil {
		return nil, failure("invalidValue", "resource must be an object")
	}
	if len(operations) == 0 {
		return nil, failure("invalidSyntax", "Operations must not be empty")
	}
	v, e := copyJSON(resource)
	if e != nil {
		return nil, e
	}
	result := v.(map[string]any)
	for _, operation := range operations {
		before := clone(result)
		op := strings.ToLower(operation.Op)
		if op != "add" && op != "remove" && op != "replace" {
			return nil, failure("invalidSyntax", "unsupported patch operation")
		}
		var value any
		if op != "remove" {
			value, e = copyJSON(operation.Value)
			if e != nil {
				return nil, e
			}
			if e = reconcilePrimary(nil, value); e != nil {
				return nil, e
			}
		}
		if operation.Path == "" {
			if op == "remove" {
				return nil, failure("noTarget", "remove requires path")
			}
			m, ok := value.(map[string]any)
			if !ok || len(m) == 0 {
				return nil, failure("invalidValue", "pathless operation requires a nonempty object")
			}
			keys := make([]string, 0, len(m))
			for k := range m {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				if strings.Contains(k, ":") {
					if _, ok := m[k].(map[string]any); ok { // A schema URI object is resource representation, not attrPath.
						if _, err := parseAttr(k + ":placeholder"); err == nil {
							if err = mergeExtension(result, k, m[k], op); err != nil {
								return nil, err
							}
							continue
						}
					}
				}
				p, err := parsePatchPath(k)
				if err != nil {
					return nil, err
				}
				if err = applyPath(result, p, op, m[k]); err != nil {
					return nil, err
				}
			}
		} else {
			p, err := parsePatchPath(operation.Path)
			if err != nil {
				return nil, err
			}
			if err = applyPath(result, p, op, value); err != nil {
				return nil, err
			}
		}
		if err := reconcilePrimary(before, result); err != nil {
			return nil, err
		}
	}
	return result, nil
}

// A newly selected primary displaces the prior primary (RFC 7644 3.5.2).
func reconcilePrimary(before, after any) error {
	switch a := after.(type) {
	case map[string]any:
		b, _ := before.(map[string]any)
		for k, v := range a {
			if err := reconcilePrimary(b[key(b, k)], v); err != nil {
				return err
			}
		}
	case []any:
		b, _ := before.([]any)
		count, newCount, selected := 0, 0, -1
		for i, v := range a {
			m, ok := v.(map[string]any)
			if !ok {
				continue
			}
			if m[key(m, "primary")] != true {
				continue
			}
			count++
			was := false
			if i < len(b) {
				if old, ok := b[i].(map[string]any); ok {
					was = old[key(old, "primary")] == true
				}
			}
			if !was {
				newCount++
				selected = i
			}
		}
		if count > 1 {
			if newCount != 1 {
				return failure("invalidValue", "multi-valued attribute has multiple primary values")
			}
			for i, v := range a {
				if m, ok := v.(map[string]any); ok && i != selected {
					if m[key(m, "primary")] == true {
						m[key(m, "primary")] = false
					}
				}
			}
		}
	}
	return nil
}
func copyJSON(v any) (any, error) {
	if _, err := json.Marshal(v); err != nil {
		return nil, failure("invalidValue", "value is not valid JSON")
	}
	return clone(v), nil
}
func clone(v any) any {
	switch x := v.(type) {
	case map[string]any:
		m := make(map[string]any, len(x))
		for k, a := range x {
			m[k] = clone(a)
		}
		return m
	case []any:
		a := make([]any, len(x))
		for i, v := range x {
			a[i] = clone(v)
		}
		return a
	default:
		return v
	}
}
func parsePatchPath(s string) (patchPath, error) {
	out := patchPath{}
	p, err := newParser(s)
	if err != nil {
		return out, failure("invalidPath", err.Error())
	}
	if len(p.tokens) == 0 {
		return out, failure("invalidPath", "empty path")
	}
	out.attr, err = parseAttr(p.take())
	if err != nil {
		return out, failure("invalidPath", err.Error())
	}
	if p.peek() == "[" {
		if p.space() {
			return out, failure("invalidPath", "space before value filter")
		}
		p.take()
		out.filter, err = p.parseOr(true)
		if err == nil {
			out.filter = contextualize(out.filter, out.attr)
		}
		if err != nil {
			return out, failure("invalidPath", err.Error())
		}
		if p.take() != "]" {
			return out, failure("invalidPath", "missing closing bracket")
		}
		if p.peek() != "" {
			if p.space() {
				return out, failure("invalidPath", "space before subattribute")
			}
			sub := p.take()
			if !strings.HasPrefix(sub, ".") || !validName(sub[1:]) {
				return out, failure("invalidPath", "invalid subattribute")
			}
			out.sub = sub[1:]
		}
	}
	if p.peek() != "" || p.tokens[0].start != 0 || p.tokens[len(p.tokens)-1].end != len(s) {
		return out, failure("invalidPath", "unexpected path token")
	}
	return out, nil
}
func coreSchema(s string) bool {
	return s == "" || strings.HasPrefix(strings.ToLower(s), "urn:ietf:params:scim:schemas:core:2.0:")
}
func checkMutable(root map[string]any, p attrPath, op string, value any) error {
	if !coreSchema(p.schema) {
		return nil
	}
	name := p.parts[0]
	if strings.EqualFold(name, "id") || strings.EqualFold(name, "meta") {
		return failure("mutability", "attribute is read-only: "+name)
	}
	if strings.EqualFold(name, "userName") {
		if _, exists := root[key(root, "userName")]; exists && (op == "remove" || !present(value)) {
			return failure("mutability", "userName is required")
		}
	}
	return nil
}
func mergeExtension(root map[string]any, schema string, value any, op string) error {
	m := value.(map[string]any)
	for k, v := range m {
		p, e := parseAttr(schema + ":" + k)
		if e != nil {
			return failure("invalidPath", e.Error())
		}
		if e = applyPath(root, patchPath{attr: p}, op, v); e != nil {
			return e
		}
	}
	return nil
}
func applyPath(root map[string]any, p patchPath, op string, value any) error {
	if err := checkMutable(root, p.attr, op, value); err != nil {
		return err
	}
	m := root
	if !coreSchema(p.attr.schema) {
		if op != "remove" {
			sk := key(root, "schemas")
			schemas, _ := root[sk].([]any)
			found := false
			for _, s := range schemas {
				if text, ok := s.(string); ok && strings.EqualFold(text, p.attr.schema) {
					found = true
					break
				}
			}
			if !found {
				root[sk] = append(schemas, p.attr.schema)
			}
		}
		k := key(root, p.attr.schema)
		v, ok := root[k]
		if !ok || v == nil {
			if op == "remove" {
				return nil
			}
			v = map[string]any{}
			root[k] = v
		}
		var valid bool
		m, valid = v.(map[string]any)
		if !valid {
			return failure("invalidPath", "schema extension must be complex")
		}
	}
	if p.filter == nil {
		_, e := mutate(m, p.attr.parts, op, value)
		return e
	}
	// Resolve the container without flattening its array, so selection retains
	// record identity and cannot accidentally match subattributes across records.
	containers, e := parents(m, p.attr.parts[:len(p.attr.parts)-1], false)
	if e != nil {
		return e
	}
	matched := false
	for _, container := range containers {
		k := key(container, p.attr.parts[len(p.attr.parts)-1])
		v, exists := container[k]
		if !exists || v == nil {
			continue
		}
		a, ok := v.([]any)
		if !ok {
			return failure("invalidPath", "valuePath requires a multi-valued attribute")
		}
		out := make([]any, 0, len(a))
		for _, record := range a {
			obj, ok := record.(map[string]any)
			if !ok || !p.filter.match(obj) {
				out = append(out, record)
				continue
			}
			matched = true
			if p.sub != "" {
				if _, e = mutate(obj, []string{p.sub}, op, value); e != nil {
					return e
				}
				out = append(out, obj)
				continue
			}
			if op == "remove" {
				continue
			}
			if op == "replace" {
				switch x := value.(type) {
				case []any:
					for _, entry := range x {
						out = append(out, clone(entry))
					}
				default:
					out = append(out, clone(value))
				}
			} else {
				if _, ok := value.(map[string]any); !ok {
					return failure("invalidValue", "adding to selected complex value requires object")
				}
				merged, e := mergeValue(obj, value, "add")
				if e != nil {
					return e
				}
				out = append(out, merged)
			}
		}
		if len(out) == 0 {
			delete(container, k)
		} else {
			container[k] = out
		}
	}
	if !matched && op != "remove" {
		return failure("noTarget", "valuePath matched no value")
	}
	return nil
}
func parents(m map[string]any, parts []string, create bool) ([]map[string]any, error) {
	if len(parts) == 0 {
		return []map[string]any{m}, nil
	}
	k := key(m, parts[0])
	v, ok := m[k]
	if !ok || v == nil {
		if !create {
			return nil, nil
		}
		v = map[string]any{}
		m[k] = v
	}
	switch x := v.(type) {
	case map[string]any:
		return parents(x, parts[1:], create)
	case []any:
		var out []map[string]any
		for _, entry := range x {
			obj, ok := entry.(map[string]any)
			if !ok {
				return nil, failure("invalidPath", "subattribute parent is not complex")
			}
			ps, e := parents(obj, parts[1:], create)
			if e != nil {
				return nil, e
			}
			out = append(out, ps...)
		}
		return out, nil
	default:
		return nil, failure("invalidPath", "subattribute parent is not complex")
	}
}
func mutate(m map[string]any, parts []string, op string, value any) (bool, error) {
	ps, e := parents(m, parts[:len(parts)-1], op != "remove")
	if e != nil {
		return false, e
	}
	changed := false
	for _, obj := range ps {
		k := key(obj, parts[len(parts)-1])
		if op == "remove" {
			if _, ok := obj[k]; ok {
				delete(obj, k)
				changed = true
			}
			continue
		}
		v, err := mergeValue(obj[k], value, op)
		if err != nil {
			return false, err
		}
		obj[k] = v
		changed = true
	}
	return changed, nil
}
func mergeValue(old, value any, op string) (any, error) {
	if existing, ok := old.(map[string]any); ok {
		if value == nil {
			return nil, nil
		}
		incoming, ok := value.(map[string]any)
		if !ok {
			return nil, failure("invalidValue", "complex attribute requires object")
		}
		for k, v := range incoming {
			actual := key(existing, k)
			merged, e := mergeValue(existing[actual], v, op)
			if e != nil {
				return nil, e
			}
			existing[actual] = merged
		}
		return existing, nil
	}
	if a, ok := old.([]any); ok && op == "add" {
		incoming, ok := value.([]any)
		if !ok {
			incoming = []any{value}
		}
		for _, v := range incoming {
			duplicate := false
			for _, current := range a {
				if reflect.DeepEqual(current, v) {
					duplicate = true
					break
				}
			}
			if !duplicate {
				a = append(a, clone(v))
			}
		}
		return a, nil
	}
	return clone(value), nil
}
