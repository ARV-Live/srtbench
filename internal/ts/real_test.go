package ts

import (
	"os"
	"testing"
)

// TestAgainstRealCapture parses an actual SRT-received MPEG-TS file if one is
// present. Synthetic fixtures prove the arithmetic; only a real muxer proves
// the parser survives real PSI, adaptation fields and PES framing.
func TestAgainstRealCapture(t *testing.T) {
	path := os.Getenv("SRTBENCH_TEST_TS")
	if path == "" {
		t.Skip("set SRTBENCH_TEST_TS to a .ts file to run")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Skip(err)
	}
	p := New()
	p.Write(b)
	s := p.Snapshot(4.0)

	t.Logf("packets=%d null=%d desyncs=%d bitrate=%.0f kbps",
		s.Packets, s.NullPackets, s.Desyncs, s.BitrateKbps)
	for _, pid := range p.PIDs() {
		t.Logf("  PID %4d %-7s pkts=%-5d ccErr=%d lost=%d pes=%d",
			pid.PID, pid.Kind, pid.Packets, pid.CCErrors, pid.CCLost, pid.PESCount)
	}
	t.Logf("video: pid=%d pkts=%d loss=%.3f%%", s.Video.PID, s.Video.Packets, s.Video.LossPct)
	t.Logf("audio: pid=%d pkts=%d loss=%.3f%%", s.Audio.PID, s.Audio.Packets, s.Audio.LossPct)
	if s.HaveAVDrift {
		t.Logf("A/V drift: %+.1f ms", s.AVDriftMS)
	}
	if s.HavePCRJitter {
		t.Logf("PCR jitter: %.3f ms", s.PCRJitterMS)
	}

	if !s.Video.Present || !s.Audio.Present {
		t.Fatal("failed to classify both video and audio from the real PMT")
	}
	if s.Desyncs > 0 {
		t.Errorf("desynced %d times on a clean capture", s.Desyncs)
	}
	if s.Video.CCErrors > 0 || s.Audio.CCErrors > 0 {
		t.Errorf("invented loss on a clean capture: video=%d audio=%d",
			s.Video.CCErrors, s.Audio.CCErrors)
	}
}
