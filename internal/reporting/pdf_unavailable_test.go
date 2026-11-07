package reporting

import (
	"bytes"
	"errors"
	"testing"

	"github.com/torvanis/janus/internal/fipsmode"
)

// TestPDFExportUnavailable pins both editions: the default edition offers and
// produces PDF; the FIPS edition neither offers it nor produces it, and says
// why with the shared fipsmode error.
func TestPDFExportUnavailable(t *testing.T) {
	var b bytes.Buffer
	err := Export(&b, fixture(), "pdf")
	hasPDF := false
	for _, f := range Formats() {
		hasPDF = hasPDF || f == "pdf"
	}
	if fipsmode.IsFIPSEdition() {
		if PDFAvailable || hasPDF {
			t.Fatal("FIPS edition still offers PDF export")
		}
		if !errors.Is(err, fipsmode.ErrUnavailable) || b.Len() != 0 {
			t.Fatalf("FIPS edition PDF export: err=%v bytes=%d", err, b.Len())
		}
		return
	}
	if !PDFAvailable || !hasPDF || err != nil || !bytes.HasPrefix(b.Bytes(), []byte("%PDF-")) {
		t.Fatalf("default edition lost PDF export: available=%v offered=%v err=%v", PDFAvailable, hasPDF, err)
	}
}
