// Package qoe implements the MOS model: a G.1070-structured video score, an
// Ie,eff-structured audio score, and their multimedia integration.
//
// The package is sans-io and, apart from the optional Smoother, stateless. It
// never calls time.Now, never touches a socket, never reads a file. That is
// not an aesthetic choice: it is what lets the calibrator replay a recorded
// CSV through the exact same code the live receiver runs, which is the only
// way fitted coefficients are guaranteed to mean the same thing in production.
package qoe

import (
	"math"
)

// Reason explains why a window was not scored. Empty means it was.
type Reason string

const (
	ReasonNone           Reason = ""
	ReasonWarmup         Reason = "warmup"
	ReasonNoConnection   Reason = "no-connection"
	ReasonConnectionLost Reason = "connection-lost"
	ReasonCounterReset   Reason = "counter-reset"
	ReasonNoTSSync       Reason = "no-ts-sync"
	ReasonUndecryptable  Reason = "undecryptable"
	ReasonToolDrop       Reason = "tool-drop"
	ReasonBadWindow      Reason = "bad-window"
	ReasonNonFinite      Reason = "nonfinite"
	ReasonNoMedia        Reason = "no-media"
)

// Sample is one measurement window. Every counter field is a DELTA over the
// window, never a lifetime total.
//
// Lifetime totals are unusable here: an SRT reconnect restarts them, and a
// delta computed as unsigned subtraction against a stale base wraps to about
// 1.8e19 and pins the MOS at 1.0 permanently. Reset detection happens before a
// Sample is built, so by this point the deltas are known-good or the window is
// already marked invalid.
type Sample struct {
	// WindowMs must come from a MONOTONIC clock. An NTP step producing a
	// negative window would otherwise divide the entire model by a negative.
	WindowMs float64

	Stream StreamProfile
	Net    NetSample
	TS     TSSample
	Video  VideoSample
	Audio  AudioSample
	Sync   SyncSample

	// WarmupRemaining > 0 means the pipeline has not stabilised yet.
	WarmupRemaining int
	// CounterReset signals that a monotonic counter went backwards.
	CounterReset bool
	// ToolDropBytes is data WE failed to hand to the decoder because our own
	// pipe backed up. It is not network loss and must never be scored as
	// such: a measurement tool that creates the impairment it is measuring is
	// worthless, so this invalidates the window and is logged loudly.
	ToolDropBytes uint64
}

// StreamProfile is what the stream IS, as opposed to how it is behaving.
// Re-read every window so an ABR change or a mid-stream resolution switch is
// picked up rather than silently scored against the wrong coefficients.
type StreamProfile struct {
	VideoCodec        string
	AudioCodec        string
	Width, Height     int
	FrameRateCoded    float64
	AudioSampleRateHz int
	AudioChannels     int

	// HasVideoTrack and HasAudioTrack come from the PMT, NOT from packet
	// presence. A one-second audio dropout is a defect (scored via the gap
	// penalty); a video-only stream is a configuration (scored as MOS =
	// video). Conflating them makes every video-only stream look broken.
	HasVideoTrack bool
	HasAudioTrack bool
	PMTSeen       bool
}

// NetSample is the SRT transport view.
type NetSample struct {
	PktRecvDelta          uint64
	PktRecvLossDelta      uint64 // initially missing; MOSTLY RECOVERED by ARQ
	PktRecvRetransDelta   uint64 // recovered by retransmission
	PktRecvDropDelta      uint64 // TSBPD deadline blown -> ACTUALLY GONE
	PktRecvUndecryptDelta uint64
	ByteRecvDelta         uint64

	MsRTT            float64
	MbpsRecvRate     float64
	MbpsLinkCapacity float64
	MsRecvTsbPdDelay float64
	MsRecvBuf        float64
	PktFlightSize    int

	// ConnectedMs is how much of the window the connection was actually up.
	ConnectedMs float64
	// AvgPayloadBytes converts SRT packet drops into TS packet counts.
	// Typically 1316 = 7 x 188.
	AvgPayloadBytes float64
}

// PIDLoss is per-PID damage accounting from the TS layer.
type PIDLoss struct {
	Present     bool
	PID         uint16
	PktsDelta   uint64
	CCErrEvents uint64
	// CCGapPkts is the SUM of ((cc - ccPrev - 1) & 0x0F) over discontinuity
	// events -- how many packets vanished, not how many times continuity
	// broke. It is a modulo-16 UNDERCOUNT; see fuseLoss.
	CCGapPkts uint64
}

// TSSample is the transport-stream view.
type TSSample struct {
	PktsTotalDelta uint64
	SyncLossEvents uint64
	Video          PIDLoss
	Audio          PIDLoss
	// AudioWin carries audio damage accumulated over AudioCoeffs.LossWindowMs
	// instead of over the scoring window. At ~179 audio TS packets/s a 1 s
	// window quantises audio loss to 0.56% steps, and one lost packet then
	// moves audio MOS by 0.42 -- a cliff that makes the series unreadable.
	// The receiver keeps a longer sliding accumulator and puts it here.
	AudioWin      PIDLoss
	AudioWinMs    float64
	PCRJitterMS   float64
	HavePCRJitter bool
	// VideoESBytes is PES payload on the video PID -- the good bitrate
	// source. See videoBitrate for why the SRT receive rate is not.
	VideoESBytes uint64
	AudioESBytes uint64
	// FramePeriodMs is the modal spacing of video PES timestamps, which
	// yields the CODING frame rate robustly under loss. See videoFrameRate.
	FramePeriodMs float64
}

// VideoSample is the decoder's view of the video track.
type VideoSample struct {
	FramesOutDelta    uint64
	FramesDupDelta    uint64
	FramesDropDelta   uint64
	DecodeErrDelta    uint64
	CorruptFrameDelta uint64
	NoFrameDelta      uint64

	FreezeEvents      uint64
	FreezeMsTotal     float64
	FreezeMsMax       float64
	MsSinceLastFreeze float64 // +Inf if never
}

// AudioSample is the decoder's view of the audio track.
type AudioSample struct {
	FramesOutDelta uint64
	DecodeErrDelta uint64

	GapEvents      uint64
	GapMsTotal     float64
	GapMsMax       float64
	MsSinceLastGap float64

	// SilenceMsTotal is DIAGNOSTIC ONLY by default. An IRL streamer who stops
	// talking is not a quality defect, and a tool that scores it as one will
	// be dismissed on first use. Only corroborated silence becomes a gap.
	SilenceMsTotal float64

	PeakDBFS       float64
	RMSDBFS        float64
	ClippedSamples uint64
	AStatsValid    bool
}

// SyncSample carries the audio/video skew.
type SyncSample struct {
	Valid bool
	// SkewMs must be the A/V offset's DEVIATION FROM ITS OWN BASELINE, not the
	// raw offset between audio and video timestamps.
	//
	// This distinction is not pedantry. MPEG-TS muxers deliberately run video
	// ahead of audio, so a perfectly in-sync stream carries a large constant
	// offset: a real ffmpeg capture measured -242 ms here. Feeding that raw
	// figure in would push EVERY healthy stream past the -185 ms saturation
	// and cost it the full penalty. The ts package learns the baseline and
	// reports the deviation; that deviation is what belongs in this field.
	//
	// SIGN CONVENTION, fixed once and never renegotiated:
	//   POSITIVE = AUDIO IS EARLY (sound leads picture).
	//   NEGATIVE = AUDIO IS LATE  (sound lags picture).
	// Reversing this silently swaps the asymmetric +40/-60 ms tolerance band,
	// and because the band is asymmetric precisely because the directions are
	// perceived differently, the error does not announce itself.
	SkewMs        float64
	Samples       int
	DriftMsPerMin float64
}

// Score is the model output for one window.
type Score struct {
	Valid  bool
	Reason Reason

	MOSOverall float64
	MOSVideo   float64
	MOSAudio   float64 // NaN when there is no audio track

	HasVideo bool
	HasAudio bool

	// Confidence in [0,1] reflects how much of the window was actually
	// observed and how well the loss sources agreed.
	Confidence float64
	// LossAgreement is 1.0 when the loss sources and the decoder agree,
	// 0.5 when loss was measured but the decoder was happy (benign), and
	// 0.3 when the decoder complained but measured loss was zero (alarming:
	// we are under-counting).
	LossAgreement float64
	// Extrapolated means an input fell outside the profile's trust region.
	Extrapolated bool
	// MOSUncorrected is the score before the calibration correction, kept so a
	// fit can always be re-derived from stored data and so the correction's
	// effect stays visible rather than baked in irreversibly.
	MOSUncorrected float64
	Corrected      bool

	// Intermediates, exported for debugging and calibration.
	BrKbps          float64 // offered (loss-corrected) video bitrate
	BrKbpsReceived  float64
	Fr              float64 // CODING frame rate
	FrDelivered     float64
	PplvPct         float64
	PplaPct         float64
	BurstRatioAudio float64
	WrapFactorK     float64

	Ofr, Iofr, DFr, DPplV, ICoding, VqCoding float64
	KFreeze, KSync, KAudioGap                float64
	RAudio, IeAudio, IeEffAudio, SqRaw       float64
}

// Model evaluates samples against a profile.
type Model struct {
	P Profile
}

// NewModel returns a model bound to a profile.
func NewModel(p Profile) *Model { return &Model{P: p} }

// Evaluate scores one window. It is pure: same Sample in, same Score out.
func (m *Model) Evaluate(s Sample) Score {
	sc := Score{Confidence: 1, LossAgreement: 1, MOSAudio: math.NaN()}

	// --- gating -----------------------------------------------------------
	switch {
	case s.WindowMs <= 0:
		return invalid(sc, ReasonBadWindow)
	case s.ToolDropBytes > 0:
		// We created this impairment. Never score it as network loss.
		return invalid(sc, ReasonToolDrop)
	case s.CounterReset:
		return invalid(sc, ReasonCounterReset)
	case s.WarmupRemaining > 0:
		return invalid(sc, ReasonWarmup)
	case s.Net.ConnectedMs <= 0:
		return invalid(sc, ReasonNoConnection)
	case s.Net.ConnectedMs < 400:
		return invalid(sc, ReasonConnectionLost)
	}
	// A wrong passphrase must not be reported as packet loss.
	if s.Net.PktRecvUndecryptDelta > 0 && s.TS.PktsTotalDelta == 0 {
		return invalid(sc, ReasonUndecryptable)
	}
	// Partial windows are scored, with proportionally reduced confidence.
	if s.Net.ConnectedMs < s.WindowMs {
		sc.Confidence *= s.Net.ConnectedMs / s.WindowMs
	}
	// Payload that never syncs as MPEG-TS: degrade to SRT-only, do not crash.
	if s.TS.PktsTotalDelta == 0 && s.Net.PktRecvDelta > 0 {
		sc.Reason = ReasonNoTSSync
		sc.Confidence = math.Min(sc.Confidence, 0.3)
		sc.Extrapolated = true
	}

	// --- loss fusion ------------------------------------------------------
	pplv, ppla, k, burstA := m.fuseLoss(s)
	sc.PplvPct, sc.PplaPct, sc.WrapFactorK, sc.BurstRatioAudio = pplv, ppla, k, burstA

	// Decoder corroboration. The alarming case is a complaining decoder with
	// zero measured loss: reporting a clean MOS over a visibly broken picture
	// is the worst failure this tool can have, so we raise a floor.
	decErrs := s.Video.DecodeErrDelta + s.Video.CorruptFrameDelta + s.Video.NoFrameDelta
	if lossSeen := pplv > 0; lossSeen != (decErrs > 0) {
		if decErrs > 0 {
			sc.LossAgreement = 0.3
			floor := 100 * math.Min(1, float64(decErrs)/math.Max(float64(s.Video.FramesOutDelta), 1)) *
				m.P.Fuse.DecoderFloorScale
			if floor > pplv {
				pplv = floor
				sc.PplvPct = pplv
			}
		} else {
			// Loss landed on a non-reference frame or an unscored PID. Benign.
			sc.LossAgreement = 0.5
		}
	}
	sc.Confidence *= 0.5 + 0.5*sc.LossAgreement

	// --- video ------------------------------------------------------------
	sc.HasVideo = s.Stream.HasVideoTrack && (s.TS.Video.PktsDelta > 0 || s.Video.FramesOutDelta > 0)
	if sc.HasVideo {
		m.video(&sc, s, pplv)
	}

	// --- audio ------------------------------------------------------------
	sc.HasAudio = s.Stream.HasAudioTrack
	if sc.HasAudio {
		m.audio(&sc, s, ppla, burstA)
	}

	if !sc.HasVideo && !sc.HasAudio {
		return invalid(sc, ReasonNoMedia)
	}

	// --- integration ------------------------------------------------------
	switch {
	case sc.HasVideo && sc.HasAudio:
		c := m.P.MM
		sc.MOSOverall = c.M1 + c.M2*sc.MOSAudio + c.M3*sc.MOSVideo + c.M4*sc.MOSAudio*sc.MOSVideo
	case sc.HasVideo:
		// NOT the video score scaled down. A missing audio track is a
		// configuration, not a zero-quality audio track.
		sc.MOSOverall = sc.MOSVideo
	default:
		sc.MOSOverall = sc.MOSAudio
	}
	sc.MOSOverall = clamp(sc.MOSOverall, 1, 5)

	// Lipsync error is neither a video defect nor an audio defect -- both
	// tracks are individually perfect -- so it applies to the combined score,
	// not to either component. Applying it to one would also corrupt that
	// component's calibration.
	sc.KSync = m.syncPenalty(s)
	sc.MOSOverall = 1 + (sc.MOSOverall-1)*sc.KSync

	// The calibration correction is applied last, to the finished combined
	// score only. Applying it per-component instead would double-count it
	// through the multimedia integration, and it was fitted against a combined
	// ground truth in any case.
	if !m.P.Correction.Identity() {
		sc.MOSUncorrected = sc.MOSOverall
		sc.MOSOverall = m.P.Correction.Apply(sc.MOSOverall)
		sc.Corrected = true
	}

	sc.Valid = true
	return sanitize(sc)
}

// video runs the G.1070 chain.
func (m *Model) video(sc *Score, s Sample, pplv float64) {
	c := m.P.Video

	sc.BrKbpsReceived = m.videoBitrate(s)
	// Loss-inflation correction, and it is NOT optional.
	//
	// Under loss, received bytes fall, so a received-bytes Br falls, so Iofr
	// falls, so MOS falls -- and then exp(-Pplv/DPplV) lowers it AGAIN for the
	// same physical event. The loss gets counted twice and the model becomes
	// far too harsh in exactly the regime you care about. Br in G.1070 means
	// the ENCODER's bitrate, a coding decision, so it must be reconstructed as
	// offered load. The 0.05 floor keeps the division finite at total loss.
	sc.BrKbps = sc.BrKbpsReceived / math.Max(1-pplv/100, 0.05)

	sc.Fr = m.videoFrameRate(s)
	if s.WindowMs > 0 {
		sc.FrDelivered = float64(s.Video.FramesOutDelta) / (s.WindowMs / 1000)
	}

	if sc.BrKbps <= 0 || sc.Fr <= 0 {
		// A parser failure must not masquerade as MOS 1.
		sc.HasVideo = false
		return
	}
	if sc.BrKbps < c.BrMinKbps || sc.BrKbps > c.BrMaxKbps ||
		sc.Fr < c.FrMin || sc.Fr > c.FrMax {
		sc.Extrapolated = true
	}

	sc.Ofr = clamp(c.V1+c.V2*sc.BrKbps, 1, c.OfrMaxFps)
	sc.Iofr = c.V3 - c.V3/(1+math.Pow(sc.BrKbps/c.V4, c.V5))
	// DFr and DPplV are floored rather than allowed to reach zero. A zero
	// DPplV collapses the exponential to 0, which is arguably correct (MOS 1),
	// but a zero DFr divides by zero and yields NaN -- and a NaN comparison
	// silently reverses every ordering downstream.
	sc.DFr = math.Max(c.V6+c.V7*sc.BrKbps, 0.05)
	sc.DPplV = math.Max(c.V10+c.V11*math.Exp(-sc.Fr/c.V8)+c.V12*math.Exp(-sc.BrKbps/c.V9), 1e-3)

	d := math.Log(sc.Fr) - math.Log(sc.Ofr)
	sc.ICoding = sc.Iofr * math.Exp(-(d*d)/(2*sc.DFr*sc.DFr))
	sc.VqCoding = clamp(1+sc.ICoding*math.Exp(-pplv/sc.DPplV), 1, 5)

	sc.KFreeze = stallK(m.P.Freeze, s.Video.FreezeMsTotal, s.WindowMs,
		s.Video.FreezeEvents, s.Video.MsSinceLastFreeze)
	sc.MOSVideo = clamp(1+(sc.VqCoding-1)*sc.KFreeze, 1, 5)
}

// VideoScore runs the G.1070 video chain directly from its three inputs.
//
// Exported so the calibrator can re-evaluate candidate coefficients without
// reconstructing a whole Sample. It is the same arithmetic the live path uses,
// which is what guarantees a fitted coefficient means the same thing in
// production as it did in the fit.
func VideoScore(c VideoCoeffs, brKbps, fr, pplvPct float64) float64 {
	if brKbps <= 0 || fr <= 0 {
		return math.NaN()
	}
	ofr := clamp(c.V1+c.V2*brKbps, 1, c.OfrMaxFps)
	iofr := c.V3 - c.V3/(1+math.Pow(brKbps/c.V4, c.V5))
	dfr := math.Max(c.V6+c.V7*brKbps, 0.05)
	dpplv := math.Max(c.V10+c.V11*math.Exp(-fr/c.V8)+c.V12*math.Exp(-brKbps/c.V9), 1e-3)
	d := math.Log(fr) - math.Log(ofr)
	icoding := iofr * math.Exp(-(d*d)/(2*dfr*dfr))
	return clamp(1+icoding*math.Exp(-pplvPct/dpplv), 1, 5)
}

// videoBitrate picks the best available bitrate source.
//
// Priority matters. PES payload on the video PID is the only clean measure.
// TS packets x184 still includes adaptation-field stuffing. The SRT receive
// rate is the WORST option: it includes audio, TS overhead, SRT headers,
// retransmissions and control traffic, so it overstates video bitrate by a
// wide and load-dependent margin.
func (m *Model) videoBitrate(s Sample) float64 {
	sec := s.WindowMs / 1000
	if sec <= 0 {
		return 0
	}
	if s.TS.VideoESBytes > 0 {
		return float64(s.TS.VideoESBytes) * 8 / sec / 1000
	}
	if s.TS.Video.PktsDelta > 0 {
		return float64(s.TS.Video.PktsDelta) * 184 * 8 / sec / 1000
	}
	if s.Net.MbpsRecvRate > 0 {
		return s.Net.MbpsRecvRate * 1000
	}
	return 0
}

// videoFrameRate returns the CODING frame rate, not the delivered one.
//
// Same double-counting trap as bitrate: freezes and drops reduce delivered
// fps, and freezes already carry their own penalty term, so feeding delivered
// fps into Icoding charges for the freeze twice.
//
// The modal spacing of video PES timestamps is the good source and is free
// from the TS parser: missing frames simply do not appear, but the spacing of
// the frames that DO arrive is preserved as a multiple of the frame period, so
// the mode is robust to loss in a way a frame count is not.
func (m *Model) videoFrameRate(s Sample) float64 {
	if s.TS.FramePeriodMs > 0 {
		return 1000 / s.TS.FramePeriodMs
	}
	if s.Stream.FrameRateCoded > 0 {
		return s.Stream.FrameRateCoded
	}
	if s.WindowMs > 0 && s.Video.FramesOutDelta > 0 {
		return float64(s.Video.FramesOutDelta) / (s.WindowMs / 1000)
	}
	return 0
}

// audio runs the Ie,eff chain.
func (m *Model) audio(sc *Score, s Sample, ppla, burst float64) {
	c := m.P.Audio
	sec := s.WindowMs / 1000
	var br float64
	if sec > 0 && s.TS.AudioESBytes > 0 {
		br = float64(s.TS.AudioESBytes) * 8 / sec / 1000
	}
	if br <= 0 {
		// Fall back to the nominal rate rather than scoring a parser gap as
		// silence-quality audio.
		br = 128
		sc.Extrapolated = true
	}
	br = br / math.Max(1-ppla/100, 0.05)

	sc.IeAudio = c.IeFloor + (c.IeCeil-c.IeFloor)/(1+math.Pow(br/c.A1, c.A2))
	if ppla > 0 {
		sc.IeEffAudio = sc.IeAudio + (c.IeMax-sc.IeAudio)*ppla/(ppla/math.Max(burst, 1)+c.Bpl)
	} else {
		sc.IeEffAudio = sc.IeAudio
	}
	sc.RAudio = clamp(c.R0-sc.IeEffAudio-c.Id, 0, 100)

	// G.107's R-to-MOS conversion. This part IS standard.
	r := sc.RAudio
	sc.SqRaw = 1 + 0.035*r + 7e-6*r*(r-60)*(100-r)
	sq := sc.SqRaw
	if c.RescaleTo5 {
		// The E-model tops out at 4.5 while G.1070's Vq tops out at 5.
		// Feeding a 1..4.5 audio score and a 1..5 video score into a bilinear
		// integration whose corners assume both span 1..5 structurally
		// under-weights audio and makes MMq(5,5)=5 unreachable. This is a
		// pragmatic linear stretch, not a perceptual result.
		sq = 1 + (sq-1)*(4.0/3.5)
	}

	gapMs := s.Audio.GapMsTotal
	if m.deadAudio(s) {
		gapMs = s.WindowMs
	}
	sc.KAudioGap = stallK(m.P.AudioGap, gapMs, s.WindowMs, s.Audio.GapEvents, s.Audio.MsSinceLastGap)
	sc.MOSAudio = clamp(1+(clamp(sq, 1, 5)-1)*sc.KAudioGap, 1, 5)
}

// deadAudio decides whether silence is a defect rather than content.
//
// Silence ALONE must never reduce MOS. The corroboration is what makes this
// safe: a live encoder producing digital silence still emits AAC frames at a
// characteristic low-but-nonzero rate, whereas a broken audio path stops
// emitting frames entirely.
func (m *Model) deadAudio(s Sample) bool {
	if s.WindowMs <= 0 || s.Audio.SilenceMsTotal/s.WindowMs <= 0.95 {
		return false
	}
	return s.Audio.FramesOutDelta == 0
}

// fuseLoss reconciles the loss sources into per-PID effective loss.
//
// Authority ruling, stated once:
//   - TS continuity counters are PRIMARY. They alone say WHICH PID was
//     damaged, and per-PID separability is unique to this source.
//   - SRT PktRecvDrop is a MAGNITUDE CORRECTOR and the sole fallback.
//   - Decoder errors are CORROBORATION ONLY and never enter the arithmetic.
//
// PktRecvLoss and PktRecvRetrans are excluded from the arithmetic entirely.
// PktRecvLoss counts packets initially missing, the overwhelming majority of
// which ARQ recovers, so using it would report catastrophic loss on a link
// that delivered a perfect picture. The difference PktRecvLoss-PktRecvRetrans
// is tempting as "residual" but is wrong at a 1 s window against 3000 ms of
// latency: recovery straddles window boundaries, so the difference is
// dominated by in-flight repairs rather than by failures. PktRecvDrop is SRT's
// own statement that it gave up, and that alone is residual loss.
func (m *Model) fuseLoss(s Sample) (pplv, ppla, k, burstAudio float64) {
	g := s.TS.Video.CCGapPkts + s.TS.Audio.CCGapPkts

	perSRT := math.Round(s.Net.AvgPayloadBytes / 188)
	perSRT = clamp(perSRT, 1, m.P.Fuse.TSPerSRTPktMax)
	d := float64(s.Net.PktRecvDropDelta+s.Net.PktRecvUndecryptDelta) * perSRT

	// The continuity counter is 4 bits, so losing exactly 16 consecutive
	// packets on a PID produces ZERO discontinuities and losing 17 looks like
	// 1. Under bursty mobile loss at 7 TS packets per SRT packet, three lost
	// SRT packets is already 21 TS packets and the CC estimate is wrong. SRT's
	// drop count has no such wrap, so it corrects the magnitude. k is only
	// ever >= 1: SRT drops are a lower bound on real loss, so the correction
	// never reduces the CC estimate.
	k = 1
	if g > 0 && d > float64(g) {
		k = math.Min(d/float64(g), m.P.Fuse.KMax)
	}

	// Audio uses the long accumulator when the receiver supplied one, for the
	// quantisation reason documented on TSSample.AudioWin.
	audio := s.TS.Audio
	if s.TS.AudioWin.Present && s.TS.AudioWin.PktsDelta > 0 {
		audio = s.TS.AudioWin
	}

	lossV, lossA := float64(s.TS.Video.CCGapPkts)*k, float64(audio.CCGapPkts)*k
	if g == 0 && d > 0 {
		// CC saw nothing but SRT gave up. The damage happened; we just cannot
		// see where. Apportion by packet share.
		total := float64(s.TS.Video.PktsDelta + s.TS.Audio.PktsDelta)
		if total > 0 {
			lossV = d * float64(s.TS.Video.PktsDelta) / total
			lossA = d * float64(s.TS.Audio.PktsDelta) / total
		}
	}

	// Denominator is OFFERED (received + lost), not received. Using received
	// alone reports 50% loss as 100%.
	if den := float64(s.TS.Video.PktsDelta) + lossV; den > 0 {
		pplv = 100 * lossV / den
	}
	if den := float64(audio.PktsDelta) + lossA; den > 0 {
		ppla = 100 * lossA / den
	}

	// Mean loss run length relative to random loss -- the E-model's BurstR,
	// free from the same data.
	burstAudio = clamp(float64(audio.CCGapPkts)/math.Max(float64(audio.CCErrEvents), 1), 1, 10)
	return pplv, ppla, k, burstAudio
}

// stallK returns the headroom multiplier for a freeze or audio gap.
//
// Why multiplicative on the headroom rather than additive:
//
//  1. It cannot leave the scale. q in [1,5] implies q' in [1,5] for any
//     K in [0,1], with no clamping. An additive penalty routinely goes below
//     1 in a bad window, and the clamp that fixes that flattens the gradient
//     to exactly zero -- so the CALIBRATOR sees no signal in precisely the
//     windows carrying the most information about these coefficients.
//  2. A freeze destroys the quality you HAD. Charging a fixed 1.2 MOS points
//     against a stream already at 1.4 is meaningless; charging a fraction of
//     its remaining headroom is not.
//  3. A 200 ms freeze on a crisp 4.5 stream is jarring; the same freeze on a
//     1.5 blocky mess is barely noticeable. Multiplicative captures that
//     interaction, additive asserts it does not exist.
//
// The structure (count, duration, recency-with-memory) follows P.1203.3's
// approach to stalling, but NOT its constants. P.1203.3 models rebuffering in
// adaptive streaming, where playback pauses and resumes having lost NO
// content. A live SRT stream freezes and then SKIPS FORWARD, so the viewer
// loses continuity AND content -- a rebuffering model would systematically
// under-penalise it. Hence: their structure, our constants, marked as
// placeholders.
func stallK(c StallCoeffs, stalledMs, windowMs float64, events uint64, msSince float64) float64 {
	if windowMs <= 0 {
		return 1
	}
	if stalledMs < c.MinStallMs && events == 0 {
		// Below the threshold a late frame is jitter, not a freeze.
		if msSince <= 0 || math.IsInf(msSince, 1) {
			return 1
		}
	}
	frac := math.Min(1, stalledMs/windowMs)
	k := math.Exp(-frac/c.F1) * math.Exp(-float64(events)/c.F2)
	// The recency term is what makes this a memory model rather than a
	// window-local one: it holds K down in the windows AFTER a freeze ends, so
	// a stream that freezes once every ten seconds never fully returns to its
	// clean score. That matches how a viewer experiences a flaky stream.
	if !math.IsInf(msSince, 1) && msSince >= 0 {
		k *= 1 - c.F4*math.Exp(-msSince/c.F3)
	}
	return clamp(k, c.KFloor, 1)
}

// syncPenalty returns the headroom multiplier for lipsync error.
func (m *Model) syncPenalty(s Sample) float64 {
	c := m.P.Sync
	if !s.Sync.Valid || s.Sync.Samples < c.MinSkewSamples {
		return 1
	}
	var excess float64
	switch {
	case s.Sync.SkewMs > c.LeadOkMs:
		excess = (s.Sync.SkewMs - c.LeadOkMs) / (c.LeadMaxMs - c.LeadOkMs)
	case s.Sync.SkewMs < c.LagOkMs:
		excess = (c.LagOkMs - s.Sync.SkewMs) / (c.LagOkMs - c.LagMaxMs)
	default:
		return 1 // inside the deadband the penalty is exactly zero
	}
	return 1 - c.PenaltyMax*clamp(excess, 0, 1)
}

// MOSFromVMAF maps a VMAF score onto the MOS scale.
//
// The naive 1+4*(vmaf/100) is rejected for four concrete reasons: VMAF is an
// SVR trained to predict DMOS on its own scale and was never linear in MOS;
// VMAF 95 and 100 are perceptually indistinguishable yet linear mapping
// manufactures a 0.2 MOS gap that then dominates the calibration residual,
// since most of a healthy run lives above 90; VMAF 5 and 25 are both
// unwatchable yet linear mapping asserts 1.2 vs 2.0; and VMAF can exceed 100
// or fall below 0, which linear mapping propagates straight off the scale.
//
// The deeper reason for fitting rather than fixing this map: its job is to
// make the ground truth COMMENSURATE with the parametric scale, so W1 and W2
// should be fitted jointly with the video coefficients on the clean subset.
// That is what lets a VMAF residual be read as a model error rather than as a
// scale mismatch.
func (m *Model) MOSFromVMAF(v float64) float64 {
	c := m.P.VMAF
	v = clamp(v, 0, 100)
	if c.Kind == VMAFMapLinear {
		return clamp(1+4*v/100, 1, 5)
	}
	return clamp(1+4/(1+math.Exp(-(v-c.W1)/c.W2)), 1, 5)
}

func invalid(sc Score, r Reason) Score {
	sc.Valid = false
	sc.Reason = r
	sc.MOSOverall, sc.MOSVideo, sc.MOSAudio = math.NaN(), math.NaN(), math.NaN()
	return sc
}

// sanitize is the last line of defence: nothing non-finite ever reaches the
// database, where it would poison every aggregate silently.
func sanitize(sc Score) Score {
	if !finite(sc.MOSOverall) ||
		(sc.HasVideo && !finite(sc.MOSVideo)) ||
		(sc.HasAudio && !finite(sc.MOSAudio)) {
		return invalid(sc, ReasonNonFinite)
	}
	return sc
}

func finite(f float64) bool { return !math.IsNaN(f) && !math.IsInf(f, 0) }

func clamp(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
