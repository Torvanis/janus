package chatfile

import (
	"bytes"
	"context"
	"fmt"
	"testing"
	"time"
)

// Regression tests for the 2026-11 security review (CHAT-01): a PDF whose
// page tree refers to itself made the parser loop for good. A goroutine
// that never returns cannot be stopped, so one small upload kept a CPU core
// busy until the process was restarted.

// rawPDF assembles a PDF from object bodies (object n is objs[n-1]) with a
// correct cross-reference table.
func rawPDF(objs ...string) []byte {
	var b bytes.Buffer
	b.WriteString("%PDF-1.4\n")
	offsets := make([]int, len(objs))
	for i, o := range objs {
		offsets[i] = b.Len()
		fmt.Fprintf(&b, "%d 0 obj\n%s\nendobj\n", i+1, o)
	}
	xref := b.Len()
	fmt.Fprintf(&b, "xref\n0 %d\n0000000000 65535 f \n", len(objs)+1)
	for _, off := range offsets {
		fmt.Fprintf(&b, "%010d 00000 n \n", off)
	}
	fmt.Fprintf(&b, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(objs)+1, xref)
	return b.Bytes()
}

func TestPDFWithACyclicPageTreeIsRefused(t *testing.T) {
	const stream = "<< /Length 44 >>\nstream\nBT /F1 12 Tf 72 720 Td (hello there) Tj ET\nendstream"
	const font = "<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>"
	cases := map[string][]byte{
		"a /Pages node that lists itself": rawPDF(
			"<< /Type /Catalog /Pages 2 0 R >>",
			"<< /Type /Pages /Kids [2 0 R] /Count 1 >>"),
		"two /Pages nodes that list each other": rawPDF(
			"<< /Type /Catalog /Pages 2 0 R >>",
			"<< /Type /Pages /Kids [3 0 R 3 0 R] /Count 2 >>",
			"<< /Type /Pages /Kids [2 0 R 2 0 R] /Count 2 >>"),
		// The parser follows /Parent to find an inherited font.
		"a page whose /Parent chain loops": rawPDF(
			"<< /Type /Catalog /Pages 2 0 R >>",
			"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
			"<< /Type /Page /Parent 4 0 R /Contents 5 0 R >>",
			"<< /Type /Pages /Parent 4 0 R >>",
			stream),
	}
	for name, data := range cases {
		done := make(chan error, 1)
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			_, err := Process(ctx, "x.pdf", data)
			done <- err
		}()
		select {
		case err := <-done:
			if err == nil {
				t.Errorf("%s: accepted", name)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("%s: the reader never returned", name)
		}
	}
	// A well-formed file still reads, through the same walk.
	good := rawPDF(
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [3 0 R] /Count 1 /Resources << /Font << /F1 5 0 R >> >> >>",
		"<< /Type /Page /Parent 2 0 R /Contents 4 0 R >>",
		stream, font)
	res, err := Process(context.Background(), "x.pdf", good)
	if err != nil || !bytes.Contains([]byte(res.Text), []byte("hello there")) {
		t.Fatalf("well-formed PDF: %v %q", err, res.Text)
	}
}
