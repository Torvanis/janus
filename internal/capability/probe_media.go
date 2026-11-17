package capability

import (
	_ "embed"
	"encoding/base64"
	"regexp"
	"strconv"
	"strings"
)

// Test media for the sound and video probes. Each has one answer that can
// only be had by perceiving the file: a model that cannot hear or watch it
// has nothing to read the answer from, and a guess is unlikely to land on
// it.
//
// Both are kept as files, made by the commands below (ffmpeg 6.1.1 as
// packaged by Ubuntu 24.04, with libflite and libx264); each command gives
// the same bytes every time it is run.
//
// The sound is a spoken number, not beeps. A clip of N beeps made in Go was
// tried first and Gemini 2.5 Flash, which hears, miscounted every variant
// (4 beeps → "3" and "7"; 3 beeps → "4", "5"), so a right count could not
// be the evidence. A spoken number was heard correctly by every Gemini
// model tried.

// probeSpokenNumber is the number the test sound says.
const probeSpokenNumber = 47

// probeSoundWAV is "forty seven" in flite's slt voice, 16-bit mono 8 kHz
// WAV, 1.2 s (19,244 bytes):
//
//	ffmpeg -f lavfi -i "flite=text='forty seven':voice=slt" -ar 8000 -ac 1 \
//	  -c:a pcm_s16le -map_metadata -1 -fflags +bitexact -flags:a +bitexact probe_47.wav
//
//go:embed probe_47.wav
var probeSoundWAV []byte

// probeVideoColor is the color that fills every frame of the test video.
const probeVideoColor = "blue"

// probeVideoMP4 is one second of solid blue, 128x128 at 8 frames per
// second, H.264 in MP4 (1,749 bytes):
//
//	ffmpeg -f lavfi -i color=c=blue:s=128x128:r=8:d=1 -c:v libx264 \
//	  -pix_fmt yuv420p -preset veryslow -crf 30 -an -map_metadata -1 \
//	  -fflags +bitexact -flags:v +bitexact -movflags +faststart probe_blue.mp4
//
//go:embed probe_blue.mp4
var probeVideoMP4 []byte

// ProbeSound is the test sound (WAV).
func ProbeSound() []byte { return probeSoundWAV }

// ProbeVideo is the test video (MP4).
func ProbeVideo() []byte { return probeVideoMP4 }

// probeSoundPart is the test sound as Chat sends attached sound: an
// OpenAI input_audio part (the Gemini adapter turns it into inlineData).
func probeSoundPart() map[string]any {
	return map[string]any{"type": "input_audio", "input_audio": map[string]any{
		"data": base64.StdEncoding.EncodeToString(probeSoundWAV), "format": "wav"}}
}

// probeVideoPart is the test video as Chat sends attached video: a
// video_url data URL (vLLM / Qwen VL; the Gemini adapter turns it into
// inlineData).
func probeVideoPart() map[string]any {
	return map[string]any{"type": "video_url", "video_url": map[string]any{
		"url": "data:video/mp4;base64," + base64.StdEncoding.EncodeToString(probeVideoMP4)}}
}

var (
	reNumber     = regexp.MustCompile(`\d+`)
	reSpelled47  = regexp.MustCompile(`\bforty[\s-]*seven\b`)
	reColorWords = regexp.MustCompile(`[a-z]+`)
)

// answerNumber reads the number a reply gives ("47", "forty-seven"). A
// reply with more than one different number gives none.
func answerNumber(text string) (int, bool) {
	text = reSpelled47.ReplaceAllString(strings.ToLower(text), "47")
	found := -1
	for _, w := range reNumber.FindAllString(text, -1) {
		n, err := strconv.Atoi(w)
		if err != nil || (found >= 0 && n != found) {
			return 0, false
		}
		found = n
	}
	return found, found >= 0
}

// namesColor reports a reply naming color as a word of its own and no
// other basic color ("Blue." counts; "blue or green" does not).
func namesColor(text, color string) bool {
	basic := map[string]bool{"red": true, "green": true, "blue": true, "yellow": true, "orange": true, "purple": true, "pink": true,
		"black": true, "white": true, "gray": true, "grey": true, "brown": true, "cyan": true, "magenta": true, "violet": true}
	seen := false
	for _, w := range reColorWords.FindAllString(strings.ToLower(text), -1) {
		switch {
		case w == color:
			seen = true
		case basic[w]:
			return false
		}
	}
	return seen
}
