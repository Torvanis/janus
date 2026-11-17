package usage

import (
	"math"
)

// Measured units.
//
// Some requests are billed by something other than tokens: images made,
// seconds of video, characters read aloud. Janus measures that quantity
// (from the provider's response where it says, else from the request) and
// prices it with the model's price line for the unit (lines.go). A request
// that was measured but whose model has no line for the unit is recorded
// as measured and unpriced: the fix is a price, not a metering change.

// Accounting methods added by measuring (see the list in cost.go).
const (
	// AccountingMeasured: the request was billed by a measured unit and the
	// model's price line for it.
	AccountingMeasured = "request_measured"
	// AccountingMeasuredUnpriced: the unit was measured but the model has no
	// price line for it, so the request is recorded at no cost. A missing
	// price, never "free".
	AccountingMeasuredUnpriced = "measured_unpriced"
	// AccountingOnResult: the provider bills this job when its result is
	// fetched and reports the cost there (xAI video). The request that
	// started it records what was asked for and costs nothing itself.
	AccountingOnResult = "billed_on_result"
)

// Where a measured quantity came from.
const (
	MeasureResponse = "response" // the provider's response said
	MeasureRequest  = "request"  // the caller asked for it
	MeasureDefault  = "default"  // the caller did not say; the provider's documented default
)

// UnitUse is one measured quantity of a billing unit.
type UnitUse struct {
	// Base is the price line base that prices it (LineImage, …).
	Base string
	// Quantity is in the unit's natural measure: images, seconds,
	// minutes, characters, pages, requests.
	Quantity float64
	// Attrs are the request's attributes a line's conditions match
	// (quality, size, resolution, audio).
	Attrs map[string]string
	// Source is MeasureResponse, MeasureRequest or MeasureDefault.
	Source string
}

// unitMeasure names each base's natural measure and how many of it one
// priced unit is (a per-million-characters price covers 1,000,000).
var unitMeasure = map[string]struct {
	name string
	per  float64
}{
	LineImage:       {"image", 1},
	LineVideoSecond: {"video_second", 1},
	LineAudioMinute: {"audio_minute", 1},
	LineCharacters:  {"character", 1_000_000},
	LinePages:       {"page", 1_000},
	LineRequest:     {"request", 1},
}

// Unit is the natural measure Quantity counts ("image", "character", …),
// as recorded on the usage event.
func (u UnitUse) Unit() string { return unitMeasure[u.Base].name }

// PriceUnit prices a measured quantity with the most specific matching
// line. ok is false when the model has no line for it.
func (r Rates) PriceUnit(u UnitUse) (nano int64, key string, ok bool) {
	m, known := unitMeasure[u.Base]
	if !known || u.Quantity <= 0 {
		return 0, "", false
	}
	price, key, ok := r.UnitPrice(u.Base, u.Attrs)
	if !ok {
		return 0, "", false
	}
	return int64(math.Round(u.Quantity * float64(price) / m.per)), key, true
}
