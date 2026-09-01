package qoe

import (
	"math"
	"testing"
)

// clean returns a Sample describing a healthy 1080p30 H.264/AAC second:
// ~2216 video and ~179 audio TS packets, matching a real measured stream.
// cleanSample is clean() under a name the other test files can use.
func cleanSample() Sample { return clean() }

func clean() Sample {
	return Sample{
		WindowMs: 1000,
		Stream: StreamProfile{
			VideoCodec: "h264", AudioCodec: "aac",
			Width: 1920, Height: 1080,
			AudioSampleRateHz: 48000, AudioChannels: 2,
			HasVideoTrack: true, HasAudioTrack: true, PMTSeen: true,
		},
		Net: NetSample{
			PktRecvDelta: 1700, ConnectedMs: 1000,
			AvgPayloadBytes: 1316, MsRTT: 12, MbpsRecvRate: 4.6,
		},
		TS: TSSample{
			PktsTotalDelta: 2395,
			Video:          PIDLoss{Present: true, PID: 256, PktsDelta: 2216},
			Audio:          PIDLoss{Present: true, PID: 257, PktsDelta: 179},
			VideoESBytes:   550000, // ~4400 kbit/s
			AudioESBytes:   16000,  // ~128 kbit/s
			FramePeriodMs:  1000.0 / 30,
		},
		Video: VideoSample{FramesOutDelta: 30, MsSinceLastFreeze: math.Inf(1)},
		Audio: AudioSample{FramesOutDelta: 47, MsSinceLastGap: math.Inf(1)},
		Sync:  SyncSample{Valid: true, SkewMs: -21, Samples: 30},
	}
}

func model(t *testing.T) *Model {
	t.Helper()
	p, err := LoadProfile("h264-1080p")
	if err != nil {
		t.Fatal(err)
	}
	return NewModel(p)
}

func TestCleanStreamScoresHigh(t *testing.T) {
	sc := model(t).Evaluate(clean())
	if !sc.Valid {
		t.Fatalf("clean stream not scored: %s", sc.Reason)
	}
	if sc.MOSOverall < 4.0 {
		t.Fatalf("clean 4400 kbit/s 1080p30 scored only %.2f", sc.MOSOverall)
	}
	if sc.MOSOverall > 5 || sc.MOSVideo > 5 || sc.MOSAudio > 5 {
		t.Fatalf("score escaped the scale: %+v", sc)
	}
}

// THE acceptance test. A MOS that does not move under injected loss means the
// model is broken, and that is the single most important thing to verify.
func TestMOSDecreasesMonotonicallyWithLoss(t *testing.T) {
	m := model(t)
	var prev float64 = 6
	for _, lostPkts := range []uint64{0, 2, 5, 11, 22, 45} {
		s := clean()
		s.TS.Video.CCGapPkts = lostPkts
		s.TS.Video.CCErrEvents = 1
		s.Net.PktRecvDropDelta = lostPkts / 7

		sc := m.Evaluate(s)
		if !sc.Valid {
			t.Fatalf("loss=%d not scored: %s", lostPkts, sc.Reason)
		}
		if sc.MOSVideo > prev {
			t.Fatalf("MOS rose with more loss: %d pkts -> %.3f (previous %.3f)",
				lostPkts, sc.MOSVideo, prev)
		}
		t.Logf("video loss %2d pkts (%.3f%%) -> MOS %.3f", lostPkts, sc.PplvPct, sc.MOSVideo)
		prev = sc.MOSVideo
	}
	if prev > 3.0 {
		t.Fatalf("45 lost packets should hurt badly, still at %.2f", prev)
	}
}

// Audio damage must not drag down the video score, and vice versa. This is the
// payoff of per-PID accounting.
func TestAudioAndVideoScoreIndependently(t *testing.T) {
	m := model(t)
	base := m.Evaluate(clean())

	audioHit := clean()
	audioHit.TS.Audio.CCGapPkts = 6
	audioHit.TS.Audio.CCErrEvents = 1
	a := m.Evaluate(audioHit)

	if a.MOSAudio >= base.MOSAudio {
		t.Fatalf("audio loss did not reduce audio MOS: %.3f -> %.3f", base.MOSAudio, a.MOSAudio)
	}
	if math.Abs(a.MOSVideo-base.MOSVideo) > 1e-9 {
		t.Fatalf("audio loss changed the VIDEO score: %.4f -> %.4f", base.MOSVideo, a.MOSVideo)
	}
}

// A missing audio track is a configuration, not a zero-quality audio track.
// Scoring it as 0 or 1 would drag every dashboard mean down by ~1.5 MOS.
func TestNoAudioTrackFallsBackToVideo(t *testing.T) {
	s := clean()
	s.Stream.HasAudioTrack = false
	s.TS.Audio = PIDLoss{}
	s.TS.AudioESBytes = 0

	sc := model(t).Evaluate(s)
	if !sc.Valid {
		t.Fatalf("video-only stream not scored: %s", sc.Reason)
	}
	if sc.HasAudio {
		t.Fatal("HasAudio true with no audio track in the PMT")
	}
	if !math.IsNaN(sc.MOSAudio) {
		t.Fatalf("MOSAudio should be NaN when absent, got %v", sc.MOSAudio)
	}
	if sc.MOSOverall != sc.MOSVideo {
		t.Fatalf("overall %.3f should equal video %.3f with no audio", sc.MOSOverall, sc.MOSVideo)
	}
}

// An unscoreable window is NOT MOS 1. Writing 1.0 for "no stream" poisons every
// dashboard aggregate with a cause that is nearly impossible to find later.
func TestUnscoreableWindowsAreInvalidNotZero(t *testing.T) {
	m := model(t)
	cases := []struct {
		name string
		mut  func(*Sample)
		want Reason
	}{
		{"no connection", func(s *Sample) { s.Net.ConnectedMs = 0 }, ReasonNoConnection},
		{"warmup", func(s *Sample) { s.WarmupRemaining = 3 }, ReasonWarmup},
		{"counter reset", func(s *Sample) { s.CounterReset = true }, ReasonCounterReset},
		{"tool drop", func(s *Sample) { s.ToolDropBytes = 4096 }, ReasonToolDrop},
		{"bad window", func(s *Sample) { s.WindowMs = 0 }, ReasonBadWindow},
		{"brief connection", func(s *Sample) { s.Net.ConnectedMs = 100 }, ReasonConnectionLost},
	}
	for _, c := range cases {
		s := clean()
		c.mut(&s)
		sc := m.Evaluate(s)
		if sc.Valid {
			t.Errorf("%s: window was scored when it should not be", c.name)
		}
		if sc.Reason != c.want {
			t.Errorf("%s: reason = %q, want %q", c.name, sc.Reason, c.want)
		}
		if !math.IsNaN(sc.MOSOverall) {
			t.Errorf("%s: MOSOverall = %v, want NaN so it is never aggregated",
				c.name, sc.MOSOverall)
		}
	}
}

// A wrong passphrase must be reported as undecryptable, never as packet loss.
func TestUndecryptableIsNotLoss(t *testing.T) {
	s := clean()
	s.Net.PktRecvUndecryptDelta = 500
	s.TS = TSSample{}

	sc := model(t).Evaluate(s)
	if sc.Reason != ReasonUndecryptable {
		t.Fatalf("reason = %q, want %q", sc.Reason, ReasonUndecryptable)
	}
}

func TestFreezeReducesScore(t *testing.T) {
	m := model(t)
	base := m.Evaluate(clean())

	s := clean()
	s.Video.FreezeEvents = 1
	s.Video.FreezeMsTotal = 400
	s.Video.MsSinceLastFreeze = 0
	sc := m.Evaluate(s)

	if sc.MOSVideo >= base.MOSVideo {
		t.Fatalf("a 400 ms freeze did not reduce MOS: %.3f -> %.3f", base.MOSVideo, sc.MOSVideo)
	}
	if sc.MOSVideo < 1 {
		t.Fatalf("freeze pushed MOS below the floor: %.3f", sc.MOSVideo)
	}
}

// Total freeze is a meaningful state, not a division error.
func TestTotalFreezeScoresLowNotInvalid(t *testing.T) {
	s := clean()
	s.Video.FramesOutDelta = 0
	s.Video.FreezeEvents = 1
	s.Video.FreezeMsTotal = 1000
	s.Video.MsSinceLastFreeze = 0

	sc := model(t).Evaluate(s)
	if !sc.Valid {
		t.Fatalf("total freeze should still score, got %s", sc.Reason)
	}
	if sc.MOSVideo > 2.0 {
		t.Fatalf("a fully frozen second scored %.2f", sc.MOSVideo)
	}
}

// Inside the tolerance band the lipsync penalty must be exactly zero, not
// merely small.
func TestSyncDeadbandIsExactlyZero(t *testing.T) {
	m := model(t)
	for _, skew := range []float64{0, 39, -59, 40, -60} {
		s := clean()
		s.Sync.SkewMs = skew
		sc := m.Evaluate(s)
		if sc.KSync != 1 {
			t.Errorf("skew %.0f ms is inside the band but KSync = %v", skew, sc.KSync)
		}
	}
	// Outside it, the penalty must bite -- and asymmetrically, since audio
	// arriving early is flagged by the ear far sooner than audio arriving late.
	early := clean()
	early.Sync.SkewMs = 200
	late := clean()
	late.Sync.SkewMs = -200
	e, l := m.Evaluate(early), m.Evaluate(late)
	if e.KSync >= 1 || l.KSync >= 1 {
		t.Fatalf("gross skew not penalised: early=%v late=%v", e.KSync, l.KSync)
	}

	mild := clean()
	mild.Sync.SkewMs = 70 // past the +40 lead limit
	mildLate := clean()
	mildLate.Sync.SkewMs = -70 // barely past the -60 lag limit
	if m.Evaluate(mild).KSync >= m.Evaluate(mildLate).KSync {
		t.Fatal("audio-early should be penalised harder than audio-late at equal magnitude")
	}
}

// The multimedia corner conditions must hold exactly, or the scale drifts
// whenever the weights are retuned.
func TestMultimediaCornersExact(t *testing.T) {
	for _, w := range []float64{0.3, 0.45, 0.6} {
		for _, i := range []float64{0, 0.02, 0.05} {
			c := DeriveMM(w, i)
			lo := c.M1 + c.M2*1 + c.M3*1 + c.M4*1
			hi := c.M1 + c.M2*5 + c.M3*5 + c.M4*25
			if math.Abs(lo-1) > 1e-9 {
				t.Errorf("w=%.2f i=%.2f: MM(1,1) = %.12f, want 1", w, i, lo)
			}
			if math.Abs(hi-5) > 1e-9 {
				t.Errorf("w=%.2f i=%.2f: MM(5,5) = %.12f, want 5", w, i, hi)
			}
		}
	}
}

// Broken video should hurt the overall score more than broken audio, at the
// shipped weighting.
func TestVideoOutweighsAudioByDefault(t *testing.T) {
	m := model(t)
	c := m.P.MM
	badAudio := c.M1 + c.M2*1 + c.M3*5 + c.M4*1*5
	badVideo := c.M1 + c.M2*5 + c.M3*1 + c.M4*5*1
	if badVideo >= badAudio {
		t.Fatalf("broken video (%.2f) should score below broken audio (%.2f)", badVideo, badAudio)
	}
}

// Under loss, received bitrate falls -- and if that fed the model directly the
// loss would be charged twice, once through a lower Iofr and again through the
// loss exponential. The offered-bitrate reconstruction prevents that.
func TestBitrateIsCorrectedForLoss(t *testing.T) {
	s := clean()
	s.TS.Video.CCGapPkts = 200
	s.TS.Video.CCErrEvents = 4
	s.TS.VideoESBytes = 500000 // depressed by the loss

	sc := model(t).Evaluate(s)
	if sc.BrKbps <= sc.BrKbpsReceived {
		t.Fatalf("offered bitrate %.0f should exceed received %.0f under loss",
			sc.BrKbps, sc.BrKbpsReceived)
	}
}

// Silence alone must never reduce MOS. An IRL streamer who stops talking is
// not a quality defect.
func TestSilenceAloneDoesNotPenalise(t *testing.T) {
	m := model(t)
	base := m.Evaluate(clean())

	quiet := clean()
	quiet.Audio.SilenceMsTotal = 1000 // fully silent, but frames still flowing
	sc := m.Evaluate(quiet)
	if math.Abs(sc.MOSAudio-base.MOSAudio) > 1e-9 {
		t.Fatalf("silence with healthy frames changed audio MOS: %.4f -> %.4f",
			base.MOSAudio, sc.MOSAudio)
	}

	// Silence WITH no decoder output is a real dropout and must be caught.
	dead := clean()
	dead.Audio.SilenceMsTotal = 1000
	dead.Audio.FramesOutDelta = 0
	dead.Audio.MsSinceLastGap = 0
	if d := m.Evaluate(dead); d.MOSAudio >= base.MOSAudio {
		t.Fatalf("dead audio not penalised: %.3f", d.MOSAudio)
	}
}

// The longer audio accumulator exists to stop one lost packet moving MOS by
// 0.4; verify it actually smooths the quantisation.
func TestAudioLossWindowReducesQuantisation(t *testing.T) {
	m := model(t)

	short := clean()
	short.TS.Audio.CCGapPkts = 1
	short.TS.Audio.CCErrEvents = 1

	long := clean()
	long.TS.Audio.CCGapPkts = 1
	long.TS.Audio.CCErrEvents = 1
	long.TS.AudioWin = PIDLoss{Present: true, PID: 257, PktsDelta: 895, CCGapPkts: 1, CCErrEvents: 1}
	long.TS.AudioWinMs = 5000

	base := m.Evaluate(clean()).MOSAudio
	dropShort := base - m.Evaluate(short).MOSAudio
	dropLong := base - m.Evaluate(long).MOSAudio

	t.Logf("one lost audio packet: 1s window drops %.3f MOS, 5s window drops %.3f", dropShort, dropLong)
	if dropLong >= dropShort {
		t.Fatalf("the long window did not reduce the quantisation cliff (%.3f vs %.3f)",
			dropLong, dropShort)
	}
}

// VMAF 100 is not MOS 5, and the mapping must never leave the scale.
func TestVMAFMappingIsBoundedAndMonotone(t *testing.T) {
	m := model(t)
	prev := -1.0
	for v := 0.0; v <= 100; v += 5 {
		got := m.MOSFromVMAF(v)
		if got < 1 || got > 5 {
			t.Fatalf("VMAF %.0f mapped to %.3f, outside 1..5", v, got)
		}
		if got < prev {
			t.Fatalf("mapping not monotone at VMAF %.0f", v)
		}
		prev = got
	}
	// Out-of-range inputs must be clamped rather than propagated.
	if got := m.MOSFromVMAF(140); got > 5 {
		t.Fatalf("VMAF 140 escaped the scale: %.3f", got)
	}
	if got := m.MOSFromVMAF(-20); got < 1 {
		t.Fatalf("VMAF -20 escaped the scale: %.3f", got)
	}
}

// Nothing non-finite may ever reach the database.
func TestNonFiniteNeverEscapes(t *testing.T) {
	m := model(t)
	for _, s := range []Sample{
		func() Sample { s := clean(); s.TS.VideoESBytes = 0; s.TS.Video.PktsDelta = 0; return s }(),
		func() Sample { s := clean(); s.TS.FramePeriodMs = 0; s.Stream.FrameRateCoded = 0; return s }(),
		func() Sample { s := clean(); s.Net.AvgPayloadBytes = 0; return s }(),
	} {
		sc := m.Evaluate(s)
		if sc.Valid && (math.IsNaN(sc.MOSOverall) || math.IsInf(sc.MOSOverall, 0)) {
			t.Fatalf("a valid score carried a non-finite MOS: %+v", sc)
		}
	}
}

// Every shipped profile must be internally consistent and honest about itself.
func TestShippedProfilesAreSaneAndLabelled(t *testing.T) {
	for name, p := range Profiles() {
		if p.Provenance.Video == OriginStandard {
			t.Errorf("%s: video coefficients claim to be standard; they are derived", name)
		}
		if p.Provenance.Calibrated() {
			t.Errorf("%s: a shipped default must not claim to be calibrated", name)
		}
		if p.Video.V4 <= 0 || p.Video.V5 <= 0 || p.Audio.A1 <= 0 {
			t.Errorf("%s: non-positive scale parameter would produce NaN", name)
		}
		if p.Audio.LossWindowMs <= 0 {
			t.Errorf("%s: audio loss window unset", name)
		}
		sc := NewModel(p).Evaluate(clean())
		if !sc.Valid || sc.MOSOverall < 1 || sc.MOSOverall > 5 {
			t.Errorf("%s: clean stream scored %.3f (valid=%v)", name, sc.MOSOverall, sc.Valid)
		}
	}
}
