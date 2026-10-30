package chatfile

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/binary"
	"hash/crc32"
	"image"
	"image/color"
	"image/png"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-pdf/fpdf"
)

func mustProcess(t *testing.T, name string, data []byte) Result {
	t.Helper()
	res, err := Process(context.Background(), name, data)
	if err != nil {
		t.Fatalf("Process(%s): %v", name, err)
	}
	return res
}

func mustReject(t *testing.T, name string, data []byte) string {
	t.Helper()
	_, err := Process(context.Background(), name, data)
	if err == nil {
		t.Fatalf("Process(%s) accepted the file", name)
	}
	if !IsReject(err) {
		t.Fatalf("Process(%s) = %v, want a user-facing refusal", name, err)
	}
	return err.Error()
}

// PDFFixture builds a one-page PDF with the given text.
func PDFFixture(t *testing.T, text string) []byte {
	t.Helper()
	p := fpdf.New("P", "mm", "A4", "")
	p.AddPage()
	p.SetFont("Helvetica", "", 14)
	p.Cell(40, 10, text)
	var buf bytes.Buffer
	if err := p.Output(&buf); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// DOCXFixture builds a minimal DOCX whose body holds the given paragraphs.
func DOCXFixture(t *testing.T, paragraphs ...string) []byte {
	t.Helper()
	var body strings.Builder
	for _, p := range paragraphs {
		body.WriteString(`<w:p><w:r><w:t xml:space="preserve">` + p + `</w:t></w:r></w:p>`)
	}
	return zipOf(t, map[string]string{
		"[Content_Types].xml": `<?xml version="1.0"?><Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"/>`,
		"word/document.xml": `<?xml version="1.0" encoding="UTF-8"?><w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"><w:body>` +
			body.String() + `</w:body></w:document>`,
	})
}

func zipOf(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, content := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func pngOf(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	img.Set(0, 0, color.RGBA{R: 255, A: 255})
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestPDFTextExtraction(t *testing.T) {
	res := mustProcess(t, "permit.pdf", PDFFixture(t, "Boundary Waters canoe permit"))
	if res.Kind != KindDocument || res.MIME != "application/pdf" {
		t.Fatalf("kind/mime = %s %s", res.Kind, res.MIME)
	}
	if !strings.Contains(strings.Join(strings.Fields(res.Text), " "), "Boundary Waters canoe permit") {
		t.Fatalf("text = %q", res.Text)
	}
	if res.EstTokens() <= 0 {
		t.Fatalf("est tokens = %d", res.EstTokens())
	}
}

func TestPDFRefusals(t *testing.T) {
	mustReject(t, "fake.pdf", []byte("hello, not a pdf"))
	mustReject(t, "broken.pdf", []byte("%PDF-1.4\n garbage without xref"))
	// A cancelled context stops extraction.
	ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	time.Sleep(time.Millisecond)
	if _, err := Process(ctx, "permit.pdf", PDFFixture(t, "late")); err == nil {
		t.Fatal("expired context still extracted")
	}
}

func TestDOCXTextExtraction(t *testing.T) {
	res := mustProcess(t, "notes.docx", DOCXFixture(t, "First paragraph about loons", "Second one"))
	if res.Kind != KindDocument || !strings.Contains(res.MIME, "wordprocessingml") {
		t.Fatalf("kind/mime = %s %s", res.Kind, res.MIME)
	}
	if res.Text != "First paragraph about loons\nSecond one\n" {
		t.Fatalf("text = %q", res.Text)
	}
}

func TestDOCXBombsAndFakes(t *testing.T) {
	// Highly compressible 60 MiB document.xml: the declared size is over the cap.
	big := `<w:document xmlns:w="w"><w:body><w:p><w:r><w:t>` + strings.Repeat("a", 60<<20) + `</w:t></w:r></w:p></w:body></w:document>`
	bomb := zipOf(t, map[string]string{"word/document.xml": big})
	if len(bomb) > 1<<20 {
		t.Fatalf("fixture is not a bomb: %d bytes", len(bomb))
	}
	if msg := mustReject(t, "bomb.docx", bomb); !strings.Contains(msg, "too large") {
		t.Fatalf("bomb refusal = %q", msg)
	}

	// A bomb whose central directory lies about the size: patch the
	// declared uncompressed size to 1 byte; the bounded read still stops it.
	liar := append([]byte(nil), bomb...)
	patchDeclaredSizes(liar, 1)
	if _, err := Process(context.Background(), "liar.docx", liar); err == nil {
		t.Fatal("lying zip bomb accepted")
	}

	// Too many entries.
	many := map[string]string{"word/document.xml": `<w:document xmlns:w="w"><w:body><w:p><w:r><w:t>x</w:t></w:r></w:p></w:body></w:document>`}
	for i := 0; i < MaxDocxEntries+1; i++ {
		many["junk/"+strings.Repeat("x", 3)+string(rune('a'+i%26))+itoa(i)] = ""
	}
	if msg := mustReject(t, "many.docx", zipOf(t, many)); !strings.Contains(msg, "too many") {
		t.Fatalf("entry-count refusal = %q", msg)
	}

	// A zip that is not a Word document.
	mustReject(t, "archive.docx", zipOf(t, map[string]string{"a.txt": "hi"}))
}

// patchDeclaredSizes rewrites the uncompressed-size field of every local and
// central directory header.
func patchDeclaredSizes(b []byte, size uint32) {
	for i := 0; i+4 <= len(b); i++ {
		switch {
		case bytes.Equal(b[i:i+4], []byte("PK\x03\x04")) && i+26 <= len(b):
			binary.LittleEndian.PutUint32(b[i+22:], size)
		case bytes.Equal(b[i:i+4], []byte("PK\x01\x02")) && i+28 <= len(b):
			binary.LittleEndian.PutUint32(b[i+24:], size)
		}
	}
}

func itoa(i int) string { return strconv.Itoa(i) }

func crc32IEEE(b []byte) uint32 { return crc32.ChecksumIEEE(b) }

func TestTextFiles(t *testing.T) {
	res := mustProcess(t, "trip.md", []byte("# Trip\n\nPortage list\r\n"))
	if res.Kind != KindDocument || res.MIME != "text/markdown" || res.Text != "# Trip\n\nPortage list\n" {
		t.Fatalf("md = %+v", res)
	}
	for _, name := range []string{"a.csv", "a.json", "a.yaml", "a.yml", "a.xml", "a.html", "a.log", "main.go", "app.tsx", "Dockerfile"} {
		mustProcess(t, name, []byte("<p>value, 1</p>\n"))
	}
	if msg := mustReject(t, "latin1.txt", []byte("caf\xe9 au lait")); !strings.Contains(msg, "UTF-8") {
		t.Fatalf("invalid UTF-8 refusal = %q", msg)
	}
	mustReject(t, "empty.txt", []byte("   \n"))
	mustReject(t, "nul.txt", []byte("abc\x00def"))
	mustReject(t, "noext", []byte("hello"))
	mustReject(t, "sheet.xlsx", []byte("hello"))

	long := strings.Repeat("é", MaxTextChars+10)
	res = mustProcess(t, "long.txt", []byte(long))
	if !res.Truncated || res.TextChars != MaxTextChars {
		t.Fatalf("truncation: truncated=%v chars=%d", res.Truncated, res.TextChars)
	}
	if res.EstTokens() != MaxTextChars/4 {
		t.Fatalf("est tokens = %d", res.EstTokens())
	}
}

func TestExecutablesAndArchivesRefusedWhateverTheName(t *testing.T) {
	cases := map[string][]byte{
		"notes.txt":   append([]byte("MZ\x90\x00\x03\x00\x00\x00"), bytes.Repeat([]byte{0}, 64)...),
		"readme.md":   append([]byte("\x7fELF\x02\x01\x01"), bytes.Repeat([]byte{0}, 64)...),
		"data.csv":    zipOf(t, map[string]string{"a.txt": "hi"}),
		"photo.png":   zipOf(t, map[string]string{"a.txt": "hi"}),
		"report.pdf":  append([]byte{0x1f, 0x8b, 0x08}, bytes.Repeat([]byte{0}, 32)...),
		"program.exe": []byte("MZ"),
		"bundle.zip":  zipOf(t, map[string]string{"a.txt": "hi"}),
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) { mustReject(t, name, data) })
	}
}

func TestImages(t *testing.T) {
	res := mustProcess(t, "dot.png", pngOf(t, 3, 2))
	if res.Kind != KindImage || res.MIME != "image/png" || res.Width != 3 || res.Height != 2 {
		t.Fatalf("png = %+v", res)
	}
	// A PNG named .jpg is refused: contents must match the extension.
	mustReject(t, "dot.jpg", pngOf(t, 3, 2))
	// Text named .png.
	mustReject(t, "fake.png", []byte("hello world"))
	mustReject(t, "pic.bmp", pngOf(t, 1, 1))
}

func TestImageDecompressionBombRefused(t *testing.T) {
	// A valid PNG whose header claims 10000×5000 (50 MP); only the header
	// is read, so no pixel memory is ever allocated.
	b := pngOf(t, 1, 1)
	binary.BigEndian.PutUint32(b[16:], 10000)
	binary.BigEndian.PutUint32(b[20:], 5000)
	// Fix the IHDR CRC so the header is well-formed.
	binary.BigEndian.PutUint32(b[29:], crc32IEEE(b[12:29]))
	if msg := mustReject(t, "huge.png", b); !strings.Contains(msg, "too large") {
		t.Fatalf("refusal = %q", msg)
	}
}
