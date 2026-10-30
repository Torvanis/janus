package chatfile

import (
	"strings"
	"testing"
)

// Sound and video files are accepted by extension when their bytes match,
// and refused when they do not (a renamed program stays a program).
func TestSoundAndVideoAttachments(t *testing.T) {
	ftyp := append([]byte{0, 0, 0, 0x18}, []byte("ftypisom\x00\x00\x02\x00isomiso2")...)
	cases := []struct {
		name string
		data []byte
		kind string
		mime string
	}{
		{"song.mp3", append([]byte("ID3\x04\x00"), make([]byte, 32)...), KindAudio, "audio/mpeg"},
		{"memo.wav", append([]byte("RIFF\x24\x00\x00\x00WAVEfmt "), make([]byte, 32)...), KindAudio, "audio/wav"},
		{"voice.m4a", ftyp, KindAudio, "audio/mp4"},
		{"voice.ogg", append([]byte("OggS"), make([]byte, 32)...), KindAudio, "audio/ogg"},
		{"take.flac", append([]byte("fLaC"), make([]byte, 32)...), KindAudio, "audio/flac"},
		{"clip.mp4", ftyp, KindVideo, "video/mp4"},
		{"clip.mov", append([]byte{0, 0, 0, 0x14}, []byte("ftypqt  \x00\x00\x00\x00qt  ")...), KindVideo, "video/quicktime"},
		{"clip.webm", append([]byte{0x1a, 0x45, 0xdf, 0xa3}, make([]byte, 32)...), KindVideo, "video/webm"},
	}
	for _, c := range cases {
		res := mustProcess(t, c.name, c.data)
		if res.Kind != c.kind || res.MIME != c.mime {
			t.Errorf("%s = %s %s, want %s %s", c.name, res.Kind, res.MIME, c.kind, c.mime)
		}
	}
	if msg := mustReject(t, "clip.mp4", []byte("just some text pretending")); !strings.Contains(msg, "do not match") {
		t.Errorf("mismatch = %q", msg)
	}
	if msg := mustReject(t, "song.mp3", append([]byte("MZ"), make([]byte, 64)...)); !strings.Contains(msg, "Programs") {
		t.Errorf("program renamed .mp3 = %q", msg)
	}
	if msg := mustReject(t, "x.avi", []byte("RIFF....AVI ")); !strings.Contains(msg, "video (MP4, MOV, WebM)") {
		t.Errorf("unsupported = %q", msg)
	}
}
