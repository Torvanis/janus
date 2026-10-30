// Package chatfile turns a file a user attached in Janus Chat into what the
// model is sent: images (PNG, JPEG, GIF, WebP) pass through for vision
// models after a header check, and documents (PDF, DOCX and text-like
// files) are reduced to plain text that is placed in the prompt in full.
//
// Everything here treats the upload as hostile. The type is decided by the
// file's bytes as well as its name (an executable or archive renamed to
// .txt is refused), image dimensions are read from the header only and
// capped so a small file cannot expand into gigabytes of pixels, DOCX
// archives are bounded in entry count and total uncompressed size, text
// must be valid UTF-8, and extraction runs under the caller's context with
// a hard cap on the characters kept.
package chatfile

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"net/http"
	"path"
	"strings"
	"unicode/utf8"

	// Register the decoders image.DecodeConfig needs.
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"

	_ "golang.org/x/image/webp"
)

// Kinds of attachment.
const (
	KindImage    = "image"
	KindDocument = "document"
	// Sound and video files people attach. Models that hear or watch get
	// them as-is; for the others Janus transcribes or describes them.
	KindAudio = "audio"
	KindVideo = "video"
)

// mediaExts are the sound and video files Chat accepts, by extension.
var mediaExts = map[string]struct{ kind, mime string }{
	".mp3": {KindAudio, "audio/mpeg"}, ".wav": {KindAudio, "audio/wav"}, ".m4a": {KindAudio, "audio/mp4"},
	".ogg": {KindAudio, "audio/ogg"}, ".oga": {KindAudio, "audio/ogg"}, ".flac": {KindAudio, "audio/flac"},
	".aac": {KindAudio, "audio/aac"}, ".opus": {KindAudio, "audio/ogg"},
	".mp4": {KindVideo, "video/mp4"}, ".m4v": {KindVideo, "video/mp4"}, ".mov": {KindVideo, "video/quicktime"},
	".webm": {KindVideo, "video/webm"},
}

// MediaMIMEs are the sound and video types served inline to their owner
// (for the players); everything else is a download.
var MediaMIMEs = map[string]bool{"audio/mpeg": true, "audio/wav": true, "audio/mp4": true, "audio/ogg": true,
	"audio/flac": true, "audio/aac": true, "video/mp4": true, "video/quicktime": true, "video/webm": true}

// mediaMagic checks a sound or video file's bytes match its type.
func mediaMagic(data []byte, mime string) bool {
	ftyp := len(data) >= 12 && string(data[4:8]) == "ftyp"
	switch mime {
	case "audio/mpeg":
		return bytes.HasPrefix(data, []byte("ID3")) || (len(data) > 1 && data[0] == 0xff && data[1]&0xe0 == 0xe0)
	case "audio/aac":
		return len(data) > 1 && data[0] == 0xff && data[1]&0xf0 == 0xf0
	case "audio/wav":
		return len(data) >= 12 && string(data[:4]) == "RIFF" && string(data[8:12]) == "WAVE"
	case "audio/ogg":
		return bytes.HasPrefix(data, []byte("OggS"))
	case "audio/flac":
		return bytes.HasPrefix(data, []byte("fLaC"))
	case "audio/mp4", "video/mp4", "video/quicktime":
		return ftyp || (mime == "video/quicktime" && len(data) >= 8 && (string(data[4:8]) == "moov" || string(data[4:8]) == "wide" || string(data[4:8]) == "mdat"))
	case "video/webm":
		return bytes.HasPrefix(data, []byte{0x1a, 0x45, 0xdf, 0xa3})
	}
	return false
}

// Limits.
const (
	// MaxImagePixels refuses images whose header declares more pixels than
	// this (decompression bombs): 40 megapixels.
	MaxImagePixels = 40_000_000
	// MaxTextChars caps the extracted text kept per document (runes).
	MaxTextChars = 400_000
	// MaxDocxUncompressed caps the total uncompressed size of a DOCX.
	MaxDocxUncompressed = 50 << 20
	// MaxDocxEntries caps the number of entries in a DOCX archive.
	MaxDocxEntries = 2000
	// CharsPerToken is the rough characters-per-token ratio used for
	// estimates shown before sending.
	CharsPerToken = 4
)

// RejectError is a refusal whose message is safe to show the user.
type RejectError struct{ Reason string }

func (e *RejectError) Error() string { return e.Reason }

func reject(format string, a ...any) error { return &RejectError{Reason: fmt.Sprintf(format, a...)} }

// IsReject reports whether err is a user-facing refusal.
func IsReject(err error) bool {
	var r *RejectError
	return errors.As(err, &r)
}

// ImageMIMEs is the image allow-list: the only types ever served back with
// an image content type.
var ImageMIMEs = map[string]bool{"image/png": true, "image/jpeg": true, "image/gif": true, "image/webp": true}

var imageExts = map[string]string{
	".png": "image/png", ".jpg": "image/jpeg", ".jpeg": "image/jpeg", ".gif": "image/gif", ".webp": "image/webp",
}

// textExts are the text-like extensions accepted as documents, with the MIME
// recorded for them. HTML and XML are kept as text: they are model input,
// never rendered.
var textExts = map[string]string{
	".txt": "text/plain", ".md": "text/markdown", ".markdown": "text/markdown", ".csv": "text/csv",
	".tsv": "text/tab-separated-values", ".json": "application/json", ".jsonl": "application/json",
	".yaml": "application/yaml", ".yml": "application/yaml", ".xml": "application/xml", ".html": "text/html",
	".htm": "text/html", ".log": "text/plain", ".ini": "text/plain", ".toml": "text/plain", ".conf": "text/plain",
	".cfg": "text/plain", ".env": "text/plain", ".sql": "text/plain", ".rst": "text/plain", ".tex": "text/plain",
	".css": "text/plain", ".scss": "text/plain", ".svg": "text/plain",
	// source code
	".go": "text/plain", ".py": "text/plain", ".js": "text/plain", ".mjs": "text/plain", ".cjs": "text/plain",
	".ts": "text/plain", ".tsx": "text/plain", ".jsx": "text/plain", ".java": "text/plain", ".kt": "text/plain",
	".kts": "text/plain", ".scala": "text/plain", ".c": "text/plain", ".h": "text/plain", ".cc": "text/plain",
	".cpp": "text/plain", ".hpp": "text/plain", ".cs": "text/plain", ".rs": "text/plain", ".rb": "text/plain",
	".php": "text/plain", ".swift": "text/plain", ".m": "text/plain", ".sh": "text/plain", ".bash": "text/plain",
	".zsh": "text/plain", ".ps1": "text/plain", ".lua": "text/plain", ".pl": "text/plain", ".r": "text/plain",
	".dart": "text/plain", ".ex": "text/plain", ".exs": "text/plain", ".erl": "text/plain", ".hs": "text/plain",
	".clj": "text/plain", ".vue": "text/plain", ".svelte": "text/plain", ".tf": "text/plain", ".proto": "text/plain",
	".graphql": "text/plain", ".gradle": "text/plain", ".mk": "text/plain", ".dockerfile": "text/plain",
}

// Special file names accepted as text even without a known extension.
var textNames = map[string]bool{"dockerfile": true, "makefile": true, "readme": true, "license": true}

const (
	mimePDF  = "application/pdf"
	mimeDOCX = "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
)

// Result is an accepted attachment.
type Result struct {
	Kind string
	MIME string
	// Text is the extracted text (documents only), capped at MaxTextChars.
	Text      string
	TextChars int64
	Truncated bool
	// Width and Height are the image's pixel dimensions (images only).
	Width, Height int
}

// EstTokens is the rough token cost: text chars / 4 for documents, a flat
// figure for images (the caller decides it; 0 here).
func (r Result) EstTokens() int64 {
	if r.Kind != KindDocument {
		return 0
	}
	return (r.TextChars + CharsPerToken - 1) / CharsPerToken
}

// Process classifies and extracts one upload. Refusals are *RejectError.
func Process(ctx context.Context, filename string, data []byte) (Result, error) {
	if len(data) == 0 {
		return Result{}, reject("That file is empty.")
	}
	ext := strings.ToLower(path.Ext(filename))
	base := strings.ToLower(path.Base(strings.ReplaceAll(filename, "\\", "/")))
	head := data
	if len(head) > 512 {
		head = head[:512]
	}
	sniffed := http.DetectContentType(head)
	if dangerous(data, sniffed) && ext != ".docx" {
		return Result{}, reject("Programs and archives cannot be attached.")
	}
	switch {
	case imageExts[ext] != "":
		return processImage(data, imageExts[ext], sniffed)
	case mediaExts[ext].kind != "":
		m := mediaExts[ext]
		if !mediaMagic(data, m.mime) {
			return Result{}, reject("That file's contents do not match its %s extension.", ext)
		}
		return Result{Kind: m.kind, MIME: m.mime}, nil
	case ext == ".pdf":
		if !bytes.HasPrefix(data, []byte("%PDF-")) {
			return Result{}, reject("That file is not a PDF.")
		}
		return processPDF(ctx, data)
	case ext == ".docx":
		return processDOCX(ctx, data)
	case textExts[ext] != "" || (ext == "" && textNames[base]):
		mime := textExts[ext]
		if mime == "" {
			mime = "text/plain"
		}
		return processText(data, mime, sniffed)
	}
	if ext == "" {
		return Result{}, reject("Files without an extension cannot be attached. Rename it, for example to .txt.")
	}
	return Result{}, reject("%s files cannot be attached. Images (PNG, JPEG, GIF, WebP), sound (MP3, WAV, M4A, OGG, FLAC), video (MP4, MOV, WebM), PDF, DOCX and text files can.", ext)
}

// dangerous reports executables and archives by their bytes, whatever the
// file is called.
func dangerous(data []byte, sniffed string) bool {
	switch sniffed {
	case "application/zip", "application/x-gzip", "application/x-rar-compressed", "application/vnd.rar",
		"application/x-7z-compressed", "application/x-msdownload", "application/wasm", "application/x-tar",
		"application/x-bzip2", "application/x-xz", "application/zstd", "application/vnd.ms-cab-compressed":
		return true
	}
	for _, magic := range [][]byte{
		[]byte("MZ"),                       // Windows PE
		[]byte("\x7fELF"),                  // ELF
		{0xfe, 0xed, 0xfa, 0xce},           // Mach-O
		{0xfe, 0xed, 0xfa, 0xcf},           // Mach-O 64
		{0xce, 0xfa, 0xed, 0xfe},           // Mach-O LE
		{0xcf, 0xfa, 0xed, 0xfe},           // Mach-O 64 LE
		{0xca, 0xfe, 0xba, 0xbe},           // Mach-O fat / Java class
		[]byte("PK\x03\x04"),               // zip
		[]byte("PK\x05\x06"),               // empty zip
		[]byte("Rar!\x1a\x07"),             // rar
		{'7', 'z', 0xbc, 0xaf, 0x27, 0x1c}, // 7z
		{0x1f, 0x8b},                       // gzip
		[]byte("BZh"),                      // bzip2
		{0xfd, '7', 'z', 'X', 'Z', 0x00},   // xz
		{0x28, 0xb5, 0x2f, 0xfd},           // zstd
		[]byte("\x00asm"),                  // wasm
		[]byte("MSCF"),                     // cab
	} {
		if bytes.HasPrefix(data, magic) {
			return true
		}
	}
	return len(data) > 262 && bytes.Equal(data[257:262], []byte("ustar")) // tar
}

func processImage(data []byte, mime, sniffed string) (Result, error) {
	if sniffed != mime {
		return Result{}, reject("That file's contents do not match its image extension.")
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return Result{}, reject("That image could not be read.")
	}
	if "image/"+format != mime {
		return Result{}, reject("That file's contents do not match its image extension.")
	}
	if cfg.Width <= 0 || cfg.Height <= 0 {
		return Result{}, reject("That image has no size.")
	}
	if int64(cfg.Width)*int64(cfg.Height) > MaxImagePixels {
		return Result{}, reject("That image is too large (%d×%d). Images up to 40 megapixels can be attached.", cfg.Width, cfg.Height)
	}
	return Result{Kind: KindImage, MIME: mime, Width: cfg.Width, Height: cfg.Height}, nil
}

func processText(data []byte, mime, sniffed string) (Result, error) {
	if !strings.HasPrefix(sniffed, "text/") && sniffed != "application/json" && sniffed != "application/octet-stream" {
		return Result{}, reject("That file is not a text file.")
	}
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))
	if !utf8.Valid(data) {
		return Result{}, reject("That file is not valid UTF-8 text.")
	}
	if bytes.IndexByte(data, 0) >= 0 {
		return Result{}, reject("That file is not a text file.")
	}
	return document(mime, string(data))
}

// document finishes a document result: normalizes line endings, caps the
// text and refuses one that holds no text at all.
func document(mime, text string) (Result, error) {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	if strings.TrimSpace(text) == "" {
		return Result{}, reject("That file has no text in it.")
	}
	res := Result{Kind: KindDocument, MIME: mime}
	if utf8.RuneCountInString(text) > MaxTextChars {
		r := []rune(text)
		text = string(r[:MaxTextChars])
		res.Truncated = true
	}
	res.Text = text
	res.TextChars = int64(utf8.RuneCountInString(text))
	return res, nil
}
