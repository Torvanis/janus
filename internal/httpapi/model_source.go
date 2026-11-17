package httpapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"strings"
	"unicode/utf8"
)

// The model a request is authorized for must be the model the provider acts
// on. The gateway reads the model from the same place the provider does (the
// body), and refuses any request where two readers could disagree about it:
// a repeated or differently-cased "model" field, a body that is not what its
// Content-Type says, or a model named outside a body that also carries one.
//
// X-Janus-Model and ?model= exist for requests that have no model in the body
// (a video job's status check, a download). They never override a body.

// requestModel is the model a /v1 request names, and the check that must keep
// running over any part of the body the gateway forwards without buffering.
type requestModel struct {
	Name string
	// FromBody says the body named the model (rather than a header, the
	// query, or nothing).
	FromBody bool
	tail     tailScanner
}

// tailScanner inspects the unbuffered remainder of a body as it is forwarded.
type tailScanner interface {
	feed(p []byte)
	violated() bool
}

// errModelFieldRepeated stops an upload whose unbuffered remainder names the
// model a second time. The provider never receives the complete body.
var errModelFieldRepeated = errors.New("the request body names the model more than once")

// errAmbiguousModel is the refusal for a body two parsers could read differently.
func errAmbiguousModel(detail string) *APIError {
	return ErrInvalidRequest("The request body must name the model exactly once, in a field called \"model\": " + detail + ".").WithParam("model")
}

// guardTail wraps the unbuffered remainder of the body so that a second model
// field arriving after the buffered part ends the upload.
func (m requestModel) guardTail(rest io.Reader) io.Reader {
	if m.tail == nil {
		return rest
	}
	return &tailGuard{r: rest, scan: m.tail}
}

type tailGuard struct {
	r    io.Reader
	scan tailScanner
}

func (g *tailGuard) Read(p []byte) (int, error) {
	n, err := g.r.Read(p)
	if n > 0 {
		g.scan.feed(p[:n])
		if g.scan.violated() {
			return 0, errModelFieldRepeated
		}
	}
	return n, err
}

// maxModelNameBytes bounds a model name read from a request.
const maxModelNameBytes = 1024

// resolveRequestModel reads the model a request names. body is the buffered
// part; overflows says more follows unbuffered.
func resolveRequestModel(body []byte, overflows bool, r *http.Request) (requestModel, *APIError) {
	hinted := r.URL.Query().Get("model")
	if hinted == "" {
		hinted = r.Header.Get("X-Janus-Model")
	}
	if len(body) == 0 {
		return requestModel{Name: hinted}, nil
	}
	contentType := r.Header.Get("Content-Type")
	mediaType, params, _ := mime.ParseMediaType(contentType)
	switch {
	case strings.Contains(strings.ToLower(contentType), "json"):
		return resolveJSONModel(body, overflows, hinted)
	case strings.HasPrefix(mediaType, "multipart/"):
		return resolveMultipartModel(body, overflows, params["boundary"], hinted)
	}
	// Any other body is opaque to the gateway. A JSON document sent under
	// another content type is refused: some servers parse it anyway, and
	// the gateway would be authorizing a model it never read.
	if trimmed := bytes.TrimLeft(body, " \t\r\n\xef\xbb\xbf"); len(trimmed) > 0 && (trimmed[0] == '{' || trimmed[0] == '[') {
		return requestModel{}, ErrInvalidRequest("Send JSON request bodies with the header Content-Type: application/json.")
	}
	return requestModel{Name: hinted}, nil
}

func resolveJSONModel(body []byte, overflows bool, hinted string) (requestModel, *APIError) {
	scan := &jsonModelScan{}
	if !overflows {
		if !utf8.Valid(body) {
			return requestModel{}, ErrInvalidRequest("The request body is not valid UTF-8 text.")
		}
		if !json.Valid(body) {
			return requestModel{}, ErrInvalidRequest("The request body is not valid JSON.")
		}
	}
	scan.feed(body)
	if !scan.object {
		return requestModel{}, ErrInvalidRequest("The request body must be a JSON object.")
	}
	if problem := scan.problem(); problem != "" {
		return requestModel{}, errAmbiguousModel(problem)
	}
	if !scan.found {
		if overflows {
			// The rest of the body is forwarded without buffering, so a
			// model named later could not be checked against the grant.
			return requestModel{}, ErrInvalidRequest("In a request body this large, put the \"model\" field first so the gateway can read it.").WithParam("model")
		}
		return requestModel{Name: hinted}, nil
	}
	if scan.value == "" {
		return requestModel{}, ErrInvalidRequest("The \"model\" field is empty. Name the model to use.").WithParam("model")
	}
	out := requestModel{Name: scan.value, FromBody: true}
	if overflows {
		out.tail = scan
	}
	return out, nil
}

func resolveMultipartModel(body []byte, overflows bool, boundary, hinted string) (requestModel, *APIError) {
	if boundary == "" {
		return requestModel{}, ErrInvalidRequest("The multipart request has no boundary in its Content-Type header.")
	}
	var (
		value string
		count int
	)
	reader := multipart.NewReader(bytes.NewReader(body), boundary)
	for {
		part, err := reader.NextRawPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			if overflows {
				break // the buffered part ends mid-upload; the tail scanner covers the rest
			}
			return requestModel{}, ErrInvalidRequest("The multipart request body is malformed.")
		}
		named, exact := dispositionNamesModel(part.Header.Values("Content-Disposition"))
		if !named {
			continue
		}
		if !exact {
			return requestModel{}, errAmbiguousModel("a form field is named like \"model\" but not exactly \"model\"")
		}
		count++
		raw, _ := io.ReadAll(io.LimitReader(part, maxModelNameBytes+1))
		if len(raw) > maxModelNameBytes {
			return requestModel{}, errAmbiguousModel("the model name is too long")
		}
		value = strings.TrimSpace(string(raw))
	}
	if count > 1 {
		return requestModel{}, errAmbiguousModel("the form has more than one \"model\" field")
	}
	if !overflows {
		if count == 0 {
			return requestModel{Name: hinted}, nil
		}
		if value == "" {
			return requestModel{}, ErrInvalidRequest("The \"model\" form field is empty. Name the model to use.").WithParam("model")
		}
		return requestModel{Name: value, FromBody: true}, nil
	}
	// Overflowing upload: the model must be in the buffered part, and an
	// independent scan of the raw bytes must agree it appears exactly once
	// there, because that scan is what keeps watching the rest.
	scan := newMultipartModelScan(boundary)
	scan.feed(body)
	if count != 1 || value == "" || scan.count != 1 || scan.variant {
		return requestModel{}, ErrInvalidRequest("In an upload this large, send the \"model\" form field before the file so the gateway can read it.").WithParam("model")
	}
	return requestModel{Name: value, FromBody: true, tail: scan}, nil
}

// dispositionNamesModel reports whether a part's Content-Disposition names the
// form field "model" (named), and whether it does so exactly (exact). A header
// the gateway cannot parse but that mentions the field counts as inexact.
func dispositionNamesModel(headers []string) (named, exact bool) {
	exact = true
	for _, h := range headers {
		_, params, err := mime.ParseMediaType(h)
		if err != nil {
			lower := strings.ToLower(h)
			if strings.Contains(lower, `name="model"`) || strings.Contains(lower, "name=model") || strings.Contains(strings.ReplaceAll(lower, "filename", ""), "name*=") {
				named, exact = true, false
			}
			continue
		}
		if name, ok := params["name"]; ok && strings.EqualFold(name, "model") {
			if named || name != "model" {
				exact = false
			}
			named = true
		}
	}
	if !named {
		return false, false
	}
	return named, exact
}

// --- JSON --------------------------------------------------------------------

// jsonModelScan walks a JSON document byte by byte and watches the keys of the
// top-level object for "model". It holds no more than one key and one value,
// so it can keep running over a body of any size as it streams past.
type jsonModelScan struct {
	started bool
	object  bool // the top-level value is an object
	depth   int
	inStr   bool
	esc     bool

	expectKey bool // at depth 1 the next string is a key
	wantValue bool // the key just read was "model": the next value is the model
	capKey    bool
	capVal    bool
	buf       []byte
	over      bool // the captured string outgrew its bound

	count     int // top-level keys that read as "model" ignoring case
	variant   bool
	nonString bool
	tooLong   bool
	found     bool
	value     string
}

const maxJSONKeyBytes = 64

func (s *jsonModelScan) violated() bool { return s.count > 1 || s.variant }

// problem describes why the body's model is ambiguous, or "" when it is not.
func (s *jsonModelScan) problem() string {
	switch {
	case s.variant:
		return "a field is named like \"model\" but not exactly \"model\""
	case s.count > 1:
		return "the \"model\" field appears more than once"
	case s.nonString:
		return "the \"model\" field must be a string"
	case s.tooLong:
		return "the model name is too long"
	}
	return ""
}

func (s *jsonModelScan) feed(p []byte) {
	for _, c := range p {
		if s.inStr {
			s.inString(c)
			continue
		}
		switch c {
		case ' ', '\t', '\r', '\n':
		case '"':
			s.started = true
			s.inStr = true
			switch {
			case s.object && s.depth == 1 && s.expectKey:
				s.expectKey, s.capKey, s.buf, s.over = false, true, s.buf[:0], false
			case s.object && s.depth == 1 && s.wantValue:
				s.wantValue, s.capVal, s.buf, s.over = false, true, s.buf[:0], false
			}
		case '{', '[':
			if !s.started {
				s.started, s.object = true, c == '{'
			}
			s.valueIsNotString()
			s.depth++
			if s.depth == 1 && s.object {
				s.expectKey = true
			}
		case '}', ']':
			s.depth--
		case ',':
			if s.depth == 1 && s.object {
				s.expectKey = true
			}
		case ':':
		default: // a number, true, false or null
			s.started = true
			s.valueIsNotString()
		}
	}
}

func (s *jsonModelScan) valueIsNotString() {
	if s.object && s.depth == 1 && s.wantValue {
		s.wantValue, s.nonString = false, true
	}
}

func (s *jsonModelScan) inString(c byte) {
	capturing := s.capKey || s.capVal
	keep := func() {
		if !capturing || s.over {
			return
		}
		limit := maxModelNameBytes * 6 // every character may be a \uXXXX escape
		if s.capKey {
			limit = maxJSONKeyBytes
		}
		if len(s.buf) >= limit {
			s.over = true
			return
		}
		s.buf = append(s.buf, c)
	}
	if s.esc {
		s.esc = false
		keep()
		return
	}
	if c == '\\' {
		s.esc = true
		keep()
		return
	}
	if c != '"' {
		keep()
		return
	}
	s.inStr = false
	if !capturing {
		return
	}
	text, ok := "", !s.over
	if ok {
		quoted := make([]byte, 0, len(s.buf)+2)
		quoted = append(append(append(quoted, '"'), s.buf...), '"')
		ok = json.Unmarshal(quoted, &text) == nil
	}
	if s.capKey {
		s.capKey = false
		// A key too long to hold cannot be "model" in any spelling.
		if ok && strings.EqualFold(text, "model") {
			s.count++
			if text != "model" {
				s.variant = true
			} else if s.count == 1 {
				s.wantValue = true
			}
		}
		return
	}
	s.capVal = false
	if !ok || len(text) > maxModelNameBytes {
		s.tooLong = true
		return
	}
	s.found, s.value = true, text
}

// --- multipart ---------------------------------------------------------------

// multipartModelScan finds part headers in a multipart stream by its boundary
// and counts the parts whose Content-Disposition names the field "model". It
// keeps only enough of the stream to see a header block that straddles two
// reads.
type multipartModelScan struct {
	delim   []byte // "--" + boundary
	carry   []byte
	started bool
	count   int
	variant bool
}

// maxPartHeaderBytes is how much of a part's header block is examined.
const maxPartHeaderBytes = 8 << 10

func newMultipartModelScan(boundary string) *multipartModelScan {
	return &multipartModelScan{delim: []byte("--" + boundary)}
}

func (m *multipartModelScan) violated() bool { return m.count > 1 || m.variant }

func (m *multipartModelScan) feed(p []byte) {
	window := make([]byte, 0, len(m.carry)+len(p))
	window = append(append(window, m.carry...), p...)
	atStreamStart := !m.started
	m.started = true
	keepFrom := -1
	for i := 0; i < len(window); {
		idx := bytes.Index(window[i:], m.delim)
		if idx < 0 {
			break
		}
		pos := i + idx
		i = pos + len(m.delim)
		// A delimiter opens the stream or starts a line. The carry always
		// begins a byte before anything still to be examined, so position 0
		// of a later window is never a line start that was missed.
		if !(pos == 0 && atStreamStart) && !(pos > 0 && window[pos-1] == '\n') {
			continue
		}
		rest := window[i:]
		if len(rest) < 2 {
			keepFrom = pos
			break
		}
		if rest[0] == '-' && rest[1] == '-' {
			continue // closing delimiter
		}
		block, complete := partHeaderBlock(rest)
		if !complete {
			keepFrom = pos
			break
		}
		m.examine(block)
	}
	switch {
	case keepFrom > 0:
		m.carry = append(m.carry[:0], window[keepFrom-1:]...)
	case keepFrom == 0:
		m.carry = append(m.carry[:0], window...)
		m.started = !atStreamStart // a delimiter at the very start is still at the start
	default:
		tail := len(m.delim) + 1
		if tail > len(window) {
			tail = len(window)
		}
		m.carry = append(m.carry[:0], window[len(window)-tail:]...)
	}
}

// partHeaderBlock returns the header block that follows a delimiter line, and
// whether it is complete in rest. A block longer than maxPartHeaderBytes is
// examined as far as that bound.
func partHeaderBlock(rest []byte) ([]byte, bool) {
	limit := len(rest)
	if limit > maxPartHeaderBytes {
		limit = maxPartHeaderBytes
	}
	view := rest[:limit]
	end := bytes.Index(view, []byte("\r\n\r\n"))
	if alt := bytes.Index(view, []byte("\n\n")); alt >= 0 && (end < 0 || alt < end) {
		end = alt
	}
	if end >= 0 {
		return view[:end], true
	}
	if len(rest) >= maxPartHeaderBytes {
		return view, true
	}
	return nil, false
}

func (m *multipartModelScan) examine(block []byte) {
	var headers []string
	for _, line := range strings.Split(string(block), "\n") {
		line = strings.TrimSpace(line)
		if len(line) > 20 && strings.EqualFold(line[:20], "content-disposition:") {
			headers = append(headers, strings.TrimSpace(line[20:]))
		}
	}
	named, exact := dispositionNamesModel(headers)
	if !named {
		// A header the line split missed (folded, oddly spaced) still counts.
		lower := strings.ToLower(string(block))
		if strings.Contains(lower, `name="model"`) || strings.Contains(lower, "name=model;") || strings.HasSuffix(strings.TrimSpace(lower), "name=model") {
			named, exact = true, false
		}
	}
	if !named {
		return
	}
	m.count++
	if !exact {
		m.variant = true
	}
}
