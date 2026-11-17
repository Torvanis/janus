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
	"github.com/torvanis/janus/internal/fipsmode"
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
	if !fipsmode.Allowed(fipsmode.FeatureEncryptedPDFRead) && pdfLooksEncrypted(data) {
		// Decrypting a PDF needs MD5 and RC4, which the Janus FIPS edition
		// does not use. Refuse before the parser would reach for them.
		return Result{}, reject("That PDF is password-protected (encrypted PDFs cannot be read in the Janus FIPS edition).")
	}
	r, err := pdf.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "encrypt") || strings.Contains(err.Error(), "password") {
			return Result{}, reject("That PDF is password-protected.")
		}
		return Result{}, reject("That PDF could not be read.")
	}
	// The page tree is walked here, not by the parser: its own lookup
	// follows /Kids and /Parent with no bound, so a file whose page tree
	// refers to itself kept a CPU core busy for good (nothing can stop a
	// goroutine that never returns).
	leaves, ok := pdfPages(ctx, r)
	if !ok {
		return Result{}, reject("That PDF could not be read.")
	}
	var b strings.Builder
	for _, p := range leaves {
		if err := ctx.Err(); err != nil {
			return Result{}, reject("That PDF took too long to read.")
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

// Bounds on a PDF page tree. Real trees are a few levels deep; the node
// budget covers maxPDFPages leaves with room for every intermediate node.
const (
	maxPDFTreeDepth = 32
	maxPDFTreeNodes = 20 * maxPDFPages
)

// pdfPages lists the pages of a document in order (at most maxPDFPages),
// refusing a page tree that is deeper or larger than any real one, which is
// what a tree that refers to itself looks like. Each page's /Parent chain is
// checked the same way, because the parser follows it to find inherited
// fonts.
func pdfPages(ctx context.Context, r *pdf.Reader) ([]pdf.Page, bool) {
	var leaves []pdf.Page
	nodes := 0
	var walk func(node pdf.Value, depth int) bool
	walk = func(node pdf.Value, depth int) bool {
		nodes++
		if depth > maxPDFTreeDepth || nodes > maxPDFTreeNodes || ctx.Err() != nil {
			return false
		}
		switch node.Key("Type").Name() {
		case "Page":
			up := node
			for i := 0; !up.IsNull(); i++ {
				if i > maxPDFTreeDepth {
					return false
				}
				up = up.Key("Parent")
			}
			leaves = append(leaves, pdf.Page{V: node})
		case "Pages":
			kids := node.Key("Kids")
			for i := 0; i < kids.Len() && len(leaves) < maxPDFPages; i++ {
				if !walk(kids.Index(i), depth+1) {
					return false
				}
			}
		}
		return true
	}
	if !walk(r.Trailer().Key("Root").Key("Pages"), 0) {
		return nil, false
	}
	return leaves, true
}

// pdfLooksEncrypted reports whether the file declares a standard security
// handler (an /Encrypt entry in a trailer or cross-reference stream).
func pdfLooksEncrypted(data []byte) bool { return bytes.Contains(data, []byte("/Encrypt")) }

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
