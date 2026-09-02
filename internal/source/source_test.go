package source

import (
	"strings"
	"testing"
)

func base() Spec {
	return Spec{
		Width: 1920, Height: 1080, FPS: 30,
		VideoCodec: "libx264", VideoKbps: 4500,
		AudioCodec: "aac", AudioKbps: 128,
		GOP: 60, Realtime: true,
	}
}

// joined makes the argument list searchable as one string, with separators so
// a match cannot straddle two arguments.
func joined(s Spec) string { return " " + strings.Join(s.Args(), " ") + " " }

func has(t *testing.T, s Spec, want string) {
	t.Helper()
	if got := joined(s); !strings.Contains(got, " "+want+" ") {
		t.Errorf("missing %q in:\n  %s", want, got)
	}
}

func hasNot(t *testing.T, s Spec, unwanted string) {
	t.Helper()
	if got := joined(s); strings.Contains(got, " "+unwanted+" ") {
		t.Errorf("unexpected %q in:\n  %s", unwanted, got)
	}
}

// A media file with no audio must still carry audio.
//
// This is the bug the fallback exists for: the synthetic source adds the tone
// as a second input, the file path did not, so -input with a silent file
// produced a video-only stream while the configuration still said audio was
// on. Nothing failed -- the audio half of the MOS simply reported "no track"
// for the whole run.
func TestSilentInputFileFallsBackToTheTone(t *testing.T) {
	s := base()
	s.Input = "clip.mp4"
	s.InputHasAudio = false

	has(t, s, SyntheticAudio)
	has(t, s, "1:a:0") // audio is mapped from the tone, not the file
	has(t, s, "0:v:0")
	has(t, s, "-c:a")
	hasNot(t, s, "-an")
	// The tone never ends, so without this the mux would continue as an
	// audio-only stream after the file ran out.
	has(t, s, "-shortest")
}

// A file that has audio keeps its own, and is mapped explicitly so that cover
// art or a second program cannot be picked instead.
func TestInputFileWithAudioKeepsIt(t *testing.T) {
	s := base()
	s.Input = "clip.mp4"
	s.InputHasAudio = true

	hasNot(t, s, SyntheticAudio)
	has(t, s, "0:v:0")
	has(t, s, "0:a:0")
	hasNot(t, s, "-shortest")
}

// -no-audio must stay video-only whatever the file contains, or the flag would
// be silently overridden by the fallback.
func TestNoAudioIsNeverOverriddenByTheFallback(t *testing.T) {
	for _, hasAudio := range []bool{false, true} {
		s := base()
		s.Input = "clip.mp4"
		s.InputHasAudio = hasAudio
		s.NoAudio = true

		has(t, s, "-an")
		hasNot(t, s, SyntheticAudio)
		hasNot(t, s, "0:a:0")
		hasNot(t, s, "1:a:0")
	}
}

// The synthetic source is unchanged: two lavfi inputs, no explicit mapping
// needed because each type appears exactly once.
func TestSyntheticSourceStillCarriesBoth(t *testing.T) {
	s := base()
	has(t, s, SyntheticAudio)
	if got := joined(s); !strings.Contains(got, SyntheticVideo+"=size=1920x1080:rate=30") {
		t.Errorf("missing synthetic video in:\n  %s", got)
	}
	hasNot(t, s, "-shortest")
}
