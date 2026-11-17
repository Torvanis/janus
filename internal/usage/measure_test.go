package usage

import "testing"

func TestPriceUnit(t *testing.T) {
	r := Rates{Lines: map[string]int64{
		LineImage:                   40_000_000,  // $0.04 an image
		LineImage + "@quality=high": 170_000_000, // $0.17
		LineImage + "@quality=high,size=1024x1536":     250_000_000, // $0.25
		LineVideoSecond + "@resolution=720p":           100_000_000, // $0.10 a second
		LineCharacters:                                 15_000_000_000,
		"rate_in_nanousd@prompt_over=200000":           4_000_000_000,
		LineVideoSecond + "@audio=yes,resolution=720p": 150_000_000,
	}}
	for _, tc := range []struct {
		name string
		use  UnitUse
		nano int64
		key  string
		ok   bool
	}{
		{"plain image", UnitUse{Base: LineImage, Quantity: 2}, 80_000_000, LineImage, true},
		{"the most specific line wins", UnitUse{Base: LineImage, Quantity: 1, Attrs: map[string]string{"quality": "high", "size": "1024x1536"}}, 250_000_000, LineImage + "@quality=high,size=1024x1536", true},
		{"a line whose conditions do not all hold is skipped", UnitUse{Base: LineImage, Quantity: 1, Attrs: map[string]string{"quality": "high", "size": "512x512"}}, 170_000_000, LineImage + "@quality=high", true},
		{"seconds at a resolution", UnitUse{Base: LineVideoSecond, Quantity: 8, Attrs: map[string]string{"resolution": "720p"}}, 800_000_000, LineVideoSecond + "@resolution=720p", true},
		{"with sound", UnitUse{Base: LineVideoSecond, Quantity: 8, Attrs: map[string]string{"resolution": "720p", "audio": "yes"}}, 1_200_000_000, LineVideoSecond + "@audio=yes,resolution=720p", true},
		{"no line for the resolution: not priced", UnitUse{Base: LineVideoSecond, Quantity: 8, Attrs: map[string]string{"resolution": "1080p"}}, 0, "", false},
		{"characters are priced per million", UnitUse{Base: LineCharacters, Quantity: 1_500}, 22_500_000, LineCharacters, true},
		{"no line for the unit at all", UnitUse{Base: LineAudioMinute, Quantity: 3}, 0, "", false},
		{"nothing measured", UnitUse{Base: LineImage, Quantity: 0}, 0, "", false},
	} {
		nano, key, ok := r.PriceUnit(tc.use)
		if nano != tc.nano || key != tc.key || ok != tc.ok {
			t.Errorf("%s: got %d %q %v, want %d %q %v", tc.name, nano, key, ok, tc.nano, tc.key, tc.ok)
		}
	}
	if got := (UnitUse{Base: LineCharacters}).Unit(); got != "character" {
		t.Errorf("unit name: %q", got)
	}
}
