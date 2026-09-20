package troubleshoot

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/torvanis/janus/internal/store"
)

// Archive layout (one directory per captured request):
//
//	<request-id>/
//	  manifest.json        metadata: usage event + capture record
//	  request/headers.json redacted request headers
//	  request/body.<ext>   request body as sent by the client
//	  response/headers.json
//	  response/body.<ext>  response body as received by the client
//
// A bulk export adds captures.jsonl at the root: one JSON object per line
// (the same document as each manifest) so Athena / DuckDB / jq can query the
// whole archive without opening every directory, and README.txt describing
// the layout.

// ArchiveFormat is recorded in every manifest so consumers can detect layout
// changes.
const ArchiveFormat = "janus-troubleshooting/1"

// Manifest is the per-request metadata document.
type Manifest struct {
	Format      string                        `json:"format"`
	GeneratedAt time.Time                     `json:"generated_at"`
	Gateway     ManifestGateway               `json:"gateway"`
	Event       *store.UsageEvent             `json:"usage_event,omitempty"`
	Capture     *store.TroubleshootingCapture `json:"capture"`
	Files       ManifestFiles                 `json:"files"`
}

// ManifestGateway identifies the build that produced the archive.
type ManifestGateway struct {
	Version string `json:"version"`
	SHA     string `json:"sha"`
}

// ManifestFiles names the body files (empty when a body was not captured).
type ManifestFiles struct {
	RequestHeaders  string `json:"request_headers"`
	RequestBody     string `json:"request_body,omitempty"`
	ResponseHeaders string `json:"response_headers"`
	ResponseBody    string `json:"response_body,omitempty"`
}

// Entry is one captured request ready to be archived.
type Entry struct {
	Event        *store.UsageEvent
	Capture      *store.TroubleshootingCapture
	RequestBody  []byte
	ResponseBody []byte
}

// Archiver writes tar.gz archives.
type Archiver struct {
	Gateway ManifestGateway
}

// WriteOne streams a single-request archive.
func (a Archiver) WriteOne(w io.Writer, e Entry) error {
	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)
	if err := a.writeEntry(tw, e, time.Now().UTC()); err != nil {
		return err
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return gz.Close()
}

// WriteMany streams a bulk archive. next yields entries until it returns
// nil; errors abort the archive (the gzip stream is left unterminated so the
// client sees a corrupt file rather than a silently partial one).
func (a Archiver) WriteMany(w io.Writer, next func() (*Entry, error)) error {
	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)
	now := time.Now().UTC()
	if err := writeFile(tw, "README.txt", []byte(readme), now); err != nil {
		return err
	}
	var lines strings.Builder
	count := 0
	for {
		e, err := next()
		if err != nil {
			return err
		}
		if e == nil {
			break
		}
		if err := a.writeEntry(tw, *e, now); err != nil {
			return err
		}
		doc, _ := json.Marshal(a.manifest(*e, now))
		lines.Write(doc)
		lines.WriteByte('\n')
		count++
	}
	if err := writeFile(tw, "captures.jsonl", []byte(lines.String()), now); err != nil {
		return err
	}
	summary, _ := json.MarshalIndent(map[string]any{
		"format": ArchiveFormat, "generated_at": now, "gateway": a.Gateway, "capture_count": count,
	}, "", "  ")
	if err := writeFile(tw, "export.json", summary, now); err != nil {
		return err
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return gz.Close()
}

func (a Archiver) manifest(e Entry, now time.Time) Manifest {
	dir := entryDir(e)
	m := Manifest{
		Format: ArchiveFormat, GeneratedAt: now, Gateway: a.Gateway, Event: e.Event, Capture: e.Capture,
		Files: ManifestFiles{
			RequestHeaders:  dir + "/request/headers.json",
			ResponseHeaders: dir + "/response/headers.json",
		},
	}
	if len(e.RequestBody) > 0 {
		m.Files.RequestBody = dir + "/request/body" + bodyExt(e.RequestBody, headerValue(e.Capture.RequestHeaders, "Content-Type"))
	}
	if len(e.ResponseBody) > 0 {
		m.Files.ResponseBody = dir + "/response/body" + bodyExt(e.ResponseBody, headerValue(e.Capture.ResponseHeaders, "Content-Type"))
	}
	return m
}

func (a Archiver) writeEntry(tw *tar.Writer, e Entry, now time.Time) error {
	if e.Capture == nil {
		return fmt.Errorf("archive entry without capture")
	}
	m := a.manifest(e, now)
	dir := entryDir(e)
	manifest, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	if err := writeFile(tw, dir+"/manifest.json", manifest, now); err != nil {
		return err
	}
	if err := writeFile(tw, m.Files.RequestHeaders, prettyJSON(e.Capture.RequestHeaders), now); err != nil {
		return err
	}
	if m.Files.RequestBody != "" {
		if err := writeFile(tw, m.Files.RequestBody, e.RequestBody, now); err != nil {
			return err
		}
	}
	if err := writeFile(tw, m.Files.ResponseHeaders, prettyJSON(e.Capture.ResponseHeaders), now); err != nil {
		return err
	}
	if m.Files.ResponseBody != "" {
		if err := writeFile(tw, m.Files.ResponseBody, e.ResponseBody, now); err != nil {
			return err
		}
	}
	return nil
}

func entryDir(e Entry) string {
	if e.Capture.RequestID != "" {
		return sanitize(e.Capture.RequestID)
	}
	return sanitize(e.Capture.UsageEventID)
}

func sanitize(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			return r
		}
		return '_'
	}, s)
}

func writeFile(tw *tar.Writer, name string, content []byte, now time.Time) error {
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(content)), ModTime: now, Typeflag: tar.TypeReg}); err != nil {
		return err
	}
	_, err := tw.Write(content)
	return err
}

func prettyJSON(raw string) []byte {
	if raw == "" {
		return []byte("{}\n")
	}
	var v any
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		return []byte(raw)
	}
	out, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return []byte(raw)
	}
	return append(out, '\n')
}

func headerValue(encoded, name string) string {
	if encoded == "" {
		return ""
	}
	var h map[string]string
	if err := json.Unmarshal([]byte(encoded), &h); err != nil {
		return ""
	}
	for k, v := range h {
		if strings.EqualFold(k, name) {
			return v
		}
	}
	return ""
}

// bodyExt picks a file extension a human can double-click: JSON when the
// body parses as JSON, .sse for event streams, .txt for other text, .bin
// otherwise.
func bodyExt(body []byte, contentType string) string {
	ct := strings.ToLower(contentType)
	switch {
	case strings.Contains(ct, "text/event-stream"):
		return ".sse"
	case json.Valid(body):
		return ".json"
	case strings.HasPrefix(ct, "text/"), strings.Contains(ct, "json"), isText(body):
		return ".txt"
	default:
		return ".bin"
	}
}

func isText(b []byte) bool {
	if len(b) > 512 {
		b = b[:512]
	}
	for _, c := range b {
		if c == 0 {
			return false
		}
	}
	return true
}

const readme = `Janus troubleshooting export
============================

Layout
------
captures.jsonl          One JSON object per captured request (same document as
                        each <request-id>/manifest.json). Load it with Athena,
                        DuckDB, jq or pandas to query the whole export.
export.json             Format version, gateway build and capture count.
<request-id>/
  manifest.json         Usage-event metering fields + capture record + file map.
  request/headers.json  Request headers (Authorization, Cookie, API keys redacted).
  request/body.*        Request body as sent by the client (if captured).
  response/headers.json Response headers.
  response/body.*       Response body as delivered to the client (if captured).
                        .json = JSON body, .sse = server-sent events stream,
                        .txt = other text, .bin = binary.

Bodies were captured under the retention policy of the troubleshooting
session named in each manifest; a capture flagged "truncated" exceeded the
session's per-body limit and holds only the leading bytes.

Athena
------
CREATE EXTERNAL TABLE janus_captures (
  format string, generated_at string,
  usage_event struct<id:string, created_at:string, user_id:string, model_name:string,
                     http_status:int, error_code:string, tokens_in:bigint, tokens_out:bigint,
                     latency_ms:int, cost_nanousd:bigint, request_id:string>,
  capture struct<id:string, session_id:string, http_status:int, error_code:string,
                 tokens_in:bigint, tokens_out:bigint, truncated:boolean, size_bytes:bigint>,
  files struct<request_body:string, response_body:string>
)
ROW FORMAT SERDE 'org.openx.data.jsonserde.JsonSerDe'
LOCATION 's3://<bucket>/<prefix-containing-captures.jsonl>/';
`
