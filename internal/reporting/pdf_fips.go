//go:build fips

package reporting

import (
	"io"

	"github.com/torvanis/janus/internal/fipsmode"
)

// PDFAvailable is false in the Janus FIPS edition; see pdf_available.go.
const PDFAvailable = false

// exportPDF is not compiled into the FIPS edition (the PDF library needs
// SHA-1, MD5 and RC4). Excel, CSV and JSON exports are unaffected.
func exportPDF(io.Writer, Result) error {
	return fipsmode.Unavailable(fipsmode.FeaturePDFExport)
}
