// Package scim provides in-memory SCIM protocol helpers. Callers must validate
// resource schemas, authorization and schema-specific mutability before storage.
package scim

import (
	"encoding/json"
	"fmt"
	"math/big"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const ErrorSchema = "urn:ietf:params:scim:api:messages:2.0:Error"

// Error is both a Go error and an RFC 7644 error response. Status is serialized
// as a JSON string, as required by SCIM.
type Error struct {
	Status   int    `json:"-"`
	ScimType string `json:"scimType,omitempty"`
	Detail   string `json:"detail"`
}

func (e *Error) Error() string { return e.Detail }
func (e *Error) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Schemas  []string `json:"schemas"`
		Status   string   `json:"status"`
		ScimType string   `json:"scimType,omitempty"`
		Detail   string   `json:"detail"`
	}{[]string{ErrorSchema}, strconv.Itoa(e.Status), e.ScimType, e.Detail})
}
func failure(kind, detail string) error { return &Error{400, kind, detail} }

// ParsePagination implements RFC 7644 defaults and lower bounds. No upper
// count cap is imposed; callers choose their advertised default. Overflow and
// malformed integers are errors, and count=0 requests only totalResults.
func ParsePagination(values url.Values, defaultCount int) (startIndex, count int, err error) {
	startIndex = 1
	count = defaultCount
	if count < 0 {
		count = 0
	}
	for _, item := range []struct {
		name string
		dst  *int
	}{{"startIndex", &startIndex}, {"count", &count}} {
		raw, exists := values[item.name]
		if !exists {
			continue
		}
		if len(raw) != 1 || raw[0] == "" {
			return 0, 0, failure("invalidValue", "invalid pagination parameter: "+item.name)
		}
		n, e := strconv.Atoi(raw[0])
		if e != nil {
			return 0, 0, failure("invalidValue", "invalid pagination integer: "+item.name)
		}
		*item.dst = n
	}
	if startIndex < 1 {
		startIndex = 1
	}
	if count < 0 {
		count = 0
	}
	return startIndex, count, nil
}

type token struct {
	text       string
	start, end int
}
type parser struct {
	tokens     []token
	pos, depth int
}
type expression interface{ match(map[string]any) bool }
type Filter struct{ root expression }

// Match is read-only and safe for concurrent use when the resource is not mutated.
// Without a schema registry, strings use SCIM's default caseExact=false;
// id, externalId and member reference values use caseExact=true. Standard
// meta.created/meta.lastModified compare chronologically. Extension attribute
// schema types and caseExact overrides require caller-side schema handling.
func (f *Filter) Match(r map[string]any) bool { return f != nil && f.root != nil && f.root.match(r) }
func ParseFilter(s string) (*Filter, error) {
	p, e := newParser(s)
	if e != nil {
		return nil, e
	}
	n, e := p.parseOr(false)
	if e != nil {
		return nil, e
	}
	if p.peek() != "" {
		return nil, failure("invalidFilter", "unexpected token")
	}
	return &Filter{n}, nil
}
func newParser(s string) (*parser, error) {
	if len(s) > 1<<20 || !utf8.ValidString(s) {
		return nil, failure("invalidFilter", "invalid or oversized filter")
	}
	p := &parser{}
	for i := 0; i < len(s); {
		if s[i] == ' ' {
			i++
			continue
		}
		start := i
		switch s[i] {
		case '(', ')', '[', ']':
			i++
		case '"':
			i++
			closed := false
			for i < len(s) {
				if s[i] == '\\' {
					i += 2
					continue
				}
				if s[i] == '"' {
					i++
					closed = true
					break
				}
				i++
			}
			if !closed || i > len(s) {
				return nil, failure("invalidFilter", "unterminated string")
			}
			var v string
			if json.Unmarshal([]byte(s[start:i]), &v) != nil {
				return nil, failure("invalidFilter", "invalid JSON string")
			}
		default:
			for i < len(s) && !strings.ContainsRune(" ()[]\"", rune(s[i])) {
				i++
			}
		}
		p.tokens = append(p.tokens, token{s[start:i], start, i})
	}
	return p, nil
}
func (p *parser) peek() string {
	if p.pos >= len(p.tokens) {
		return ""
	}
	return strings.ToLower(p.tokens[p.pos].text)
}
func (p *parser) take() string {
	if p.pos >= len(p.tokens) {
		return ""
	}
	s := p.tokens[p.pos].text
	p.pos++
	return s
}
func (p *parser) space() bool {
	return p.pos > 0 && p.pos < len(p.tokens) && p.tokens[p.pos-1].end < p.tokens[p.pos].start
}
func (p *parser) parseOr(inValue bool) (expression, error) {
	l, e := p.parseAnd(inValue)
	for e == nil && p.peek() == "or" {
		if !p.space() {
			return nil, failure("invalidFilter", "missing space")
		}
		p.take()
		if !p.space() {
			return nil, failure("invalidFilter", "missing space")
		}
		var r expression
		r, e = p.parseAnd(inValue)
		l = logic{"or", l, r}
	}
	return l, e
}
func (p *parser) parseAnd(inValue bool) (expression, error) {
	l, e := p.primary(inValue)
	for e == nil && p.peek() == "and" {
		if !p.space() {
			return nil, failure("invalidFilter", "missing space")
		}
		p.take()
		if !p.space() {
			return nil, failure("invalidFilter", "missing space")
		}
		var r expression
		r, e = p.primary(inValue)
		l = logic{"and", l, r}
	}
	return l, e
}
func (p *parser) primary(inValue bool) (expression, error) {
	p.depth++
	defer func() { p.depth-- }()
	if p.depth > 128 {
		return nil, failure("invalidFilter", "filter nesting too deep")
	}
	negate := false
	if p.peek() == "not" {
		p.take()
		negate = true
		if p.peek() != "(" {
			return nil, failure("invalidFilter", "not requires parentheses")
		}
	}
	if p.peek() == "(" {
		p.take()
		n, e := p.parseOr(inValue)
		if e != nil {
			return nil, e
		}
		if p.take() != ")" {
			return nil, failure("invalidFilter", "missing closing parenthesis")
		}
		if negate {
			return logic{"not", n, nil}, nil
		}
		return n, nil
	}
	path, e := parseAttr(p.take())
	if e != nil {
		return nil, failure("invalidFilter", e.Error())
	}
	if p.peek() == "[" {
		if p.space() {
			return nil, failure("invalidFilter", "space before value filter")
		}
		if inValue {
			return nil, failure("invalidFilter", "nested valuePath is not allowed")
		}
		p.take()
		n, e := p.parseOr(true)
		if e != nil {
			return nil, e
		}
		if p.take() != "]" {
			return nil, failure("invalidFilter", "missing closing bracket")
		}
		return valueExpr{path, contextualize(n, path)}, nil
	}
	if !p.space() {
		return nil, failure("invalidFilter", "missing attribute operator")
	}
	op := strings.ToLower(p.take())
	if op == "pr" {
		return compareExpr{path: path, op: op}, nil
	}
	switch op {
	case "eq", "ne", "co", "sw", "ew", "gt", "ge", "lt", "le":
	default:
		return nil, failure("invalidFilter", "unknown comparison operator")
	}
	if !p.space() {
		return nil, failure("invalidFilter", "missing comparison value")
	}
	raw := p.take()
	var v any
	d := json.NewDecoder(strings.NewReader(raw))
	d.UseNumber()
	if raw == "" || !json.Valid([]byte(raw)) || d.Decode(&v) != nil {
		return nil, failure("invalidFilter", "comparison requires JSON scalar")
	}
	switch v.(type) {
	case nil, bool:
		if op != "eq" && op != "ne" {
			return nil, failure("invalidFilter", "operator is not valid for boolean or null")
		}
	case string:
	case json.Number:
		if _, ok := number(v); !ok {
			return nil, failure("invalidFilter", "number outside supported precision range")
		}
		if op == "co" || op == "sw" || op == "ew" {
			return nil, failure("invalidFilter", "string operator requires string")
		}
	default:
		return nil, failure("invalidFilter", "comparison requires scalar")
	}
	return compareExpr{path: path, op: op, value: v}, nil
}

type attrPath struct {
	schema string
	parts  []string
}

func validName(s string) bool {
	if s == "" || !letter(s[0]) {
		return false
	}
	for i := 1; i < len(s); i++ {
		c := s[i]
		if !letter(c) && !(c >= '0' && c <= '9') && c != '-' && c != '_' {
			return false
		}
	}
	return true
}
func letter(c byte) bool { return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' }
func parseAttr(s string) (attrPath, error) {
	p := attrPath{}
	if i := strings.LastIndex(s, ":"); i >= 0 {
		p.schema = s[:i]
		s = s[i+1:]
		u, e := url.Parse(p.schema)
		if e != nil || u.Scheme == "" || strings.ContainsAny(p.schema, " \t\r\n") {
			return p, fmt.Errorf("invalid schema URI")
		}
	}
	p.parts = strings.Split(s, ".")
	if len(p.parts) > 2 {
		return p, fmt.Errorf("attribute paths allow one subattribute")
	}
	for _, v := range p.parts {
		if !validName(v) {
			return p, fmt.Errorf("invalid attribute name")
		}
	}
	return p, nil
}
func key(m map[string]any, s string) string {
	if _, ok := m[s]; ok {
		return s
	}
	for k := range m {
		if strings.EqualFold(k, s) {
			return k
		}
	}
	return s
}
func lookup(m map[string]any, p attrPath) []any {
	var v any = m
	if p.schema != "" {
		if x, ok := m[key(m, p.schema)]; ok {
			v = x
		} else if !strings.HasPrefix(strings.ToLower(p.schema), "urn:ietf:params:scim:schemas:core:2.0:") {
			return nil
		}
	}
	return descend(v, p.parts)
}
func descend(v any, parts []string) []any {
	if a, ok := v.([]any); ok {
		var out []any
		for _, x := range a {
			out = append(out, descend(x, parts)...)
		}
		return out
	}
	if len(parts) == 0 {
		return []any{v}
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	x, ok := m[key(m, parts[0])]
	if !ok {
		return nil
	}
	return descend(x, parts[1:])
}

type logic struct {
	op   string
	l, r expression
}

func (n logic) match(m map[string]any) bool {
	switch n.op {
	case "not":
		return !n.l.match(m)
	case "and":
		return n.l.match(m) && n.r.match(m)
	default:
		return n.l.match(m) || n.r.match(m)
	}
}

type valueExpr struct {
	path attrPath
	n    expression
}

func (n valueExpr) match(m map[string]any) bool {
	for _, v := range lookup(m, n.path) {
		if x, ok := v.(map[string]any); ok && n.n.match(x) {
			return true
		}
	}
	return false
}

// Reference values in the standard members attribute are caseExact.
func contextualize(n expression, parent attrPath) expression {
	switch x := n.(type) {
	case compareExpr:
		if coreSchema(parent.schema) && strings.EqualFold(parent.parts[0], "members") && strings.EqualFold(x.path.parts[0], "value") {
			x.exact = true
		}
		return x
	case logic:
		x.l = contextualize(x.l, parent)
		if x.r != nil {
			x.r = contextualize(x.r, parent)
		}
		return x
	}
	return n
}

type compareExpr struct {
	exact bool
	path  attrPath
	op    string
	value any
}

func present(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case string:
		return x != ""
	case []any:
		for _, a := range x {
			if present(a) {
				return true
			}
		}
		return false
	case map[string]any:
		for _, a := range x {
			if present(a) {
				return true
			}
		}
		return false
	default:
		return true
	}
}
func (n compareExpr) match(m map[string]any) bool {
	vs := lookup(m, n.path)
	if len(vs) == 0 {
		vs = []any{nil}
	}
	exact := n.exact || strings.EqualFold(n.path.parts[len(n.path.parts)-1], "id") || strings.EqualFold(n.path.parts[len(n.path.parts)-1], "externalId") || (coreSchema(n.path.schema) && len(n.path.parts) == 2 && strings.EqualFold(n.path.parts[0], "members") && strings.EqualFold(n.path.parts[1], "value"))
	for _, v := range vs {
		if n.op == "pr" {
			if present(v) {
				return true
			}
			continue
		}
		dateTime := coreSchema(n.path.schema) && len(n.path.parts) == 2 && strings.EqualFold(n.path.parts[0], "meta") && (strings.EqualFold(n.path.parts[1], "created") || strings.EqualFold(n.path.parts[1], "lastModified"))
		if comparison(v, n.value, n.op, exact, dateTime) {
			return true
		}
	}
	return false
}
func number(v any) (*big.Rat, bool) {
	var s string
	switch x := v.(type) {
	case json.Number:
		s = string(x)
	case float64:
		s = strconv.FormatFloat(x, 'g', -1, 64)
	case float32:
		s = strconv.FormatFloat(float64(x), 'g', -1, 32)
	case int:
		s = strconv.Itoa(x)
	case int64:
		s = strconv.FormatInt(x, 10)
	default:
		return nil, false
	}
	if len(s) > 10000 {
		return nil, false
	}
	if i := strings.IndexAny(s, "eE"); i >= 0 {
		e, err := strconv.Atoi(s[i+1:])
		if err != nil || e > 10000 || e < -10000 {
			return nil, false
		}
	}
	n, ok := new(big.Rat).SetString(s)
	return n, ok
}
func comparison(a, b any, op string, exact, dateTime bool) bool {
	if op == "eq" || op == "ne" {
		if a == "" {
			a = nil
		}
		if b == "" {
			b = nil
		}
	}
	if a == nil || b == nil {
		eq := a == nil && b == nil
		if op == "eq" {
			return eq
		}
		return op == "ne" && !eq
	}
	cmp := 0
	switch x := a.(type) {
	case string:
		y, ok := b.(string)
		if !ok {
			return false
		}
		tx, ex := time.Parse(time.RFC3339Nano, x)
		ty, ey := time.Parse(time.RFC3339Nano, y)
		if dateTime && ex == nil && ey == nil && (op == "eq" || op == "ne" || op == "gt" || op == "ge" || op == "lt" || op == "le") {
			cmp = tx.Compare(ty)
		} else {
			if !exact {
				x = strings.ToLower(x)
				y = strings.ToLower(y)
			}
			switch op {
			case "co":
				return strings.Contains(x, y)
			case "sw":
				return strings.HasPrefix(x, y)
			case "ew":
				return strings.HasSuffix(x, y)
			}
			cmp = strings.Compare(x, y)
		}
	case bool:
		y, ok := b.(bool)
		if !ok {
			return false
		}
		if op == "eq" {
			return x == y
		}
		return op == "ne" && x != y
	default:
		nx, ok := number(a)
		if !ok {
			return false
		}
		ny, ok := number(b)
		if !ok {
			return false
		}
		cmp = nx.Cmp(ny)
	}
	switch op {
	case "eq":
		return cmp == 0
	case "ne":
		return cmp != 0
	case "gt":
		return cmp > 0
	case "ge":
		return cmp >= 0
	case "lt":
		return cmp < 0
	case "le":
		return cmp <= 0
	}
	return false
}
