package chatfile

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"

	"github.com/ledongthuc/pdf"
)

// maxPDFPages bounds how many pages are read; a header can claim any count.
const maxPDFPages = 5000

// processPDF extracts the text of every page, checking ctx between pages
// and stopping once the character cap is reached. The parser is a pure-Go
// port of rsc.io/pdf; it panics on some malformed files, so panics become a
// refusal.
func processPDF(ctx context.Context, data []byte) (res Result, err error) {
	defer func() {
		if p := recover(); p != nil {
			res, err = Result{}, reject("That PDF could not be read.")
		}
	}()
	r, err := pdf.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "encrypt") || strings.Contains(err.Error(), "password") {
			return Result{}, reject("That PDF is password-protected.")
		}
		return Result{}, reject("That PDF could not be read.")
	}
	pages := min(r.NumPage(), maxPDFPages)
	var b strings.Builder
	for i := 1; i <= pages; i++ {
		if err := ctx.Err(); err != nil {
			return Result{}, reject("That PDF took too long to read.")
		}
		p := r.Page(i)
		if p.V.IsNull() {
			continue
		}
		text, err := p.GetPlainText(nil)
		if err != nil {
			continue // one unreadable page does not sink the document
		}
		if b.Len() > 0 && text != "" {
			b.WriteString("\n\n")
		}
		b.WriteString(text)
		if b.Len() > MaxTextChars*utf8.UTFMax {
			break // document() trims to the exact rune cap
		}
	}
	text := strings.ToValidUTF8(b.String(), "")
	if strings.TrimSpace(text) == "" {
		return Result{}, reject("That PDF has no text layer (it may be scanned). Attach it as images instead.")
	}
	return document(mimePDF, text)
}

// processDOCX reads word/document.xml from the archive: paragraphs become
// lines, tabs and breaks are kept. The archive is bounded in entry count and
// in total uncompressed size, both as declared and as actually read.
func processDOCX(ctx context.Context, data []byte) (Result, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return Result{}, reject("That DOCX file could not be read.")
	}
	if len(zr.File) > MaxDocxEntries {
		return Result{}, reject("That DOCX file has too many parts.")
	}
	var total uint64
	var doc *zip.File
	for _, f := range zr.File {
		total += f.UncompressedSize64
		if f.Name == "word/document.xml" {
			doc = f
		}
	}
	if total > MaxDocxUncompressed {
		return Result{}, reject("That DOCX file is too large once unpacked.")
	}
	if doc == nil {
		return Result{}, reject("That file is not a Word document.")
	}
	rc, err := doc.Open()
	if err != nil {
		return Result{}, reject("That DOCX file could not be read.")
	}
	defer func() { _ = rc.Close() }()
	// Never trust the declared size: read at most the cap plus one byte.
	body, err := io.ReadAll(io.LimitReader(rc, MaxDocxUncompressed+1))
	if err != nil {
		return Result{}, reject("That DOCX file could not be read.")
	}
	if len(body) > MaxDocxUncompressed {
		return Result{}, reject("That DOCX file is too large once unpacked.")
	}
	text, err := docxText(ctx, body)
	if err != nil {
		if IsReject(err) {
			return Result{}, err
		}
		return Result{}, reject("That DOCX file could not be read.")
	}
	return document(mimeDOCX, text)
}

// docxText walks WordprocessingML: w:t is text, w:tab a tab, w:br/w:cr a
// line break, and the end of each w:p a newline.
func docxText(ctx context.Context, body []byte) (string, error) {
	dec := xml.NewDecoder(bytes.NewReader(body))
	var b strings.Builder
	inText := false
	for n := 0; ; n++ {
		if n%4096 == 0 && ctx.Err() != nil {
			return "", reject("That DOCX file took too long to read.")
		}
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", fmt.Errorf("docx xml: %w", err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "t":
				inText = true
			case "tab":
				b.WriteByte('\t')
			case "br", "cr":
				b.WriteByte('\n')
			}
		case xml.EndElement:
			switch t.Name.Local {
			case "t":
				inText = false
			case "p":
				b.WriteByte('\n')
			}
		case xml.CharData:
			if inText {
				b.Write(t)
			}
		}
		if b.Len() > MaxTextChars*4 {
			break
		}
	}
	return b.String(), nil
}
