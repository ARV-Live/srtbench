package qoe

import "time"

// Origin records how a coefficient was arrived at. This type exists because
// shipping a guess that looks like a measurement is the single most damaging
// thing a quality model can do, and six months later nobody remembers which
// was which.
type Origin uint8

const (
	// OriginPlaceholder is a structural guess. Not validated. It ships as a
	// default only because something must.
	OriginPlaceholder Origin = iota
	// OriginEstimated was derived from stated anchor points; the derivation
	// is written out in defaults.go so it can be redone or disputed.
	OriginEstimated
	// OriginStandard is verbatim from a published standard.
	OriginStandard
	// OriginFitted came from a least-squares fit against local full-reference
	// ground truth.
	OriginFitted
)

// MarshalYAML writes the name rather than the enum's integer value.
//
// A provenance file is read by people, and "video: 1" tells them nothing. It is
// also fragile: the integer is positional, so inserting a constant would
// silently reinterpret every profile ever written.
func (o Origin) MarshalYAML() (any, error) { return o.String(), nil }

// UnmarshalYAML accepts the name, and an unknown one degrades to placeholder --
// the safest reading, since it never overstates how well a number is known.
func (o *Origin) UnmarshalYAML(f func(any) error) error {
	var s string
	if err := f(&s); err != nil {
		return err
	}
	switch s {
	case "fitted":
		*o = OriginFitted
	case "standard":
		*o = OriginStandard
	case "estimated":
		*o = OriginEstimated
	default:
		*o = OriginPlaceholder
	}
	return nil
}

func (o Origin) String() string {
	switch o {
	case OriginEstimated:
		return "estimated"
	case OriginStandard:
		return "standard"
	case OriginFitted:
		return "fitted"
	}
	return "placeholder"
}

// Provenance carries per-block origin so the honesty survives into the
// database and onto the dashboard.
type Provenance struct {
	Video      Origin `yaml:"video"`
	Audio      Origin `yaml:"audio"`
	Correction Origin `yaml:"correction"`
	Stall      Origin `yaml:"stall"`
	Sync       Origin `yaml:"sync"`
	MM         Origin `yaml:"mm"`
	VMAF       Origin `yaml:"vmaf"`

	FittedAt    time.Time `yaml:"fitted_at,omitempty"`
	FittedN     int       `yaml:"fitted_n,omitempty"`
	FittedCells int       `yaml:"fitted_cells,omitempty"`
	// FittedRMSE is the HELD-OUT error, never in-sample. In-sample RMSE on
	// autocorrelated windows looks spectacular and means nothing.
	FittedRMSE float64 `yaml:"fitted_rmse,omitempty"`
	Notes      string  `yaml:"notes,omitempty"`
}

// Calibrated reports whether any block has actually been fitted. Emitted as a
// field so a dashboard can never quietly present a guess as a measurement.
func (p Provenance) Calibrated() bool {
	return p.Video == OriginFitted || p.Audio == OriginFitted ||
		p.Correction == OriginFitted
}

// VideoCoeffs holds the G.1070-structured video chain.
//
// UNITS ARE LOAD-BEARING. Br is kbit/s and Fr is fps, everywhere, always.
// V2, V4, V7, V9, V11 and V12 are all expressed in those units; switching Br
// to Mbit/s silently rescales five of them and the model keeps returning
// plausible-looking numbers.
type VideoCoeffs struct {
	// Each coefficient carries its OWN tag. A Go struct tag on a multi-name
	// field applies to every name in that field, so "V1, V2 float64" with one
	// tag gives BOTH the key v1 -- which makes the encoder reject the struct as
	// having a duplicate key, and would have silently mis-loaded every second
	// coefficient on the way back in.
	V1  float64 `yaml:"v1"` // Ofr  = V1 + V2*Br
	V2  float64 `yaml:"v2"`
	V3  float64 `yaml:"v3"` // Iofr = V3 - V3/(1+(Br/V4)^V5)
	V4  float64 `yaml:"v4"`
	V5  float64 `yaml:"v5"`
	V6  float64 `yaml:"v6"` // DFr  = V6 + V7*Br
	V7  float64 `yaml:"v7"`
	V8  float64 `yaml:"v8"` // DPplV = V10 + V11*e^(-Fr/V8) + V12*e^(-Br/V9)
	V9  float64 `yaml:"v9"`
	V10 float64 `yaml:"v10"`
	V11 float64 `yaml:"v11"`
	V12 float64 `yaml:"v12"`

	// OfrMaxFps clamps the optimal frame rate. THIS IS A DELIBERATE DEVIATION
	// FROM G.1070. Ofr = V1 + V2*Br is linear and unbounded, so at 6000 kbit/s
	// it predicts an optimal 48 fps and a perfectly good 30 fps stream gets
	// penalised for not being 48 fps. G.1070 never hit this because it was
	// fitted below 1 Mbit/s.
	OfrMaxFps float64 `yaml:"ofr_max_fps"`

	// Trust region. Outside it the model still answers, but the Score is
	// marked Extrapolated: that is the model's opinion, not its knowledge.
	BrMinKbps float64 `yaml:"br_min_kbps"`
	BrMaxKbps float64 `yaml:"br_max_kbps"`
	FrMin     float64 `yaml:"fr_min"`
	FrMax     float64 `yaml:"fr_max"`
}

// AudioCoeffs keeps the E-model's Ie,eff structure but not its narrowband
// speech coefficients. See defaults.go for why the narrowband model must not
// be applied verbatim to AAC-LC carrying music and ambience.
type AudioCoeffs struct {
	R0 float64 `yaml:"r0"` // signal-to-noise ceiling on the R scale

	// Ie(Br) = IeFloor + (IeCeil-IeFloor)/(1+(Br/A1)^A2)
	IeFloor float64 `yaml:"ie_floor"`
	IeCeil  float64 `yaml:"ie_ceil"`
	A1      float64 `yaml:"a1"`
	A2      float64 `yaml:"a2"`

	// Ie_eff = Ie + (IeMax-Ie)*Ppl/(Ppl/BurstR + Bpl)
	Bpl   float64 `yaml:"bpl"` // loss robustness; LARGER = MORE ROBUST
	IeMax float64 `yaml:"ie_max"`

	// Id is the delay impairment, and it is deliberately zero. The E-model's
	// Id measures conversational difficulty, and nobody is holding a two-way
	// call over a 3000 ms one-way SRT link. Folding latency into MOS would
	// make a correctly-configured high-latency link look broken. Latency is
	// reported as its own KPI instead.
	Id float64 `yaml:"id"`

	// RescaleTo5 lifts the E-model's 4.5 MOS ceiling onto the 1..5 scale the
	// video model uses. Without it the integration compares a 1..4.5 number
	// against a 1..5 one and structurally under-weights audio.
	RescaleTo5 bool `yaml:"rescale_to_5"`

	// LossWindowMs is the sliding window audio loss is accumulated over,
	// SEPARATE from the scoring window, and it is not a tuning nicety -- it
	// fixes a quantisation artifact that would otherwise make the audio
	// series unreadable.
	//
	// A muxed 1080p H.264/AAC stream carries ~2216 video TS packets/s against
	// ~179 audio (a 12:1 ratio, measured). So in a 1 s window the smallest
	// non-zero audio loss is 1/180 = 0.56%, which alone moves audio MOS from
	// 4.95 to 4.53 -- a 0.42 MOS cliff caused by ONE packet. The series would
	// be visibly bimodal and effectively uninterpretable.
	//
	// Accumulating over 5 s gives ~895 packets, a 0.11% step and a ~0.065 MOS
	// quantum. Video needs no such help: at 2216 packets/s its 1 s quantum is
	// already 0.045%, twelve times finer.
	LossWindowMs float64 `yaml:"loss_window_ms"`

	BrMinKbps float64 `yaml:"br_min_kbps"`
	BrMaxKbps float64 `yaml:"br_max_kbps"`
}

// StallCoeffs is used for both video freeze and audio gap. The penalty
// multiplies the MOS headroom: q' = 1 + (q-1)*K.
type StallCoeffs struct {
	F1 float64 `yaml:"f1"` // stalled-fraction-of-window scale
	F2 float64 `yaml:"f2"` // stall count scale, events per window
	F3 float64 `yaml:"f3"` // recency scale, ms
	F4 float64 `yaml:"f4"` // maximum recency weight, [0,1)

	// MinStallMs is the output gap that counts as a stall at all. Below it a
	// late frame is jitter, not a freeze.
	MinStallMs float64 `yaml:"min_stall_ms"`
	// KFloor stops a catastrophic window pinning K at exactly zero, which
	// would make every catastrophic window indistinguishable.
	KFloor float64 `yaml:"k_floor"`
}

// SyncCoeffs describes the lipsync tolerance band.
type SyncCoeffs struct {
	// Deadband in ms of audio-minus-video skew, positive = audio early.
	// Inside this band the penalty is EXACTLY zero, not merely small.
	LeadOkMs float64 `yaml:"lead_ok_ms"` // +40  (EBU R37)
	LagOkMs  float64 `yaml:"lag_ok_ms"`  // -60  (EBU R37)
	// Saturation, from ITU-R BT.1359-1 acceptability thresholds.
	LeadMaxMs float64 `yaml:"lead_max_ms"` // +90
	LagMaxMs  float64 `yaml:"lag_max_ms"`  // -185

	PenaltyMax     float64 `yaml:"penalty_max"`
	MinSkewSamples int     `yaml:"min_skew_samples"`
}

// MultimediaCoeffs combines the audio and video scores.
type MultimediaCoeffs struct {
	M1 float64 `yaml:"m1"`
	M2 float64 `yaml:"m2"`
	M3 float64 `yaml:"m3"`
	M4 float64 `yaml:"m4"`

	// AudioWeight and Interaction are the two free parameters; M1..M4 are
	// derived from them by DeriveMM so the corner conditions hold exactly.
	AudioWeight float64 `yaml:"audio_weight"`
	Interaction float64 `yaml:"interaction"`
}

// DeriveMM recomputes M1..M4 from AudioWeight and Interaction so that both
// corner conditions hold EXACTLY:
//
//	MMq(1,1) = 1     MMq(5,5) = 5
//
// Solving  M1+M2+M3+M4 = 1  and  M1+5*M2+5*M3+25*M4 = 5, with M2 = w*S and
// M3 = (1-w)*S, gives S = 1-6m and M1 = 5m for interaction m. Because it is
// exact, the corners never drift when the weights are retuned.
func DeriveMM(audioWeight, interaction float64) MultimediaCoeffs {
	m := interaction
	s := 1 - 6*m
	return MultimediaCoeffs{
		M1:          5 * m,
		M2:          audioWeight * s,
		M3:          (1 - audioWeight) * s,
		M4:          m,
		AudioWeight: audioWeight,
		Interaction: interaction,
	}
}

// VMAFMapKind selects the VMAF-to-MOS transfer function.
type VMAFMapKind uint8

const (
	VMAFMapLogistic VMAFMapKind = iota
	VMAFMapLinear
)

// VMAFMapCoeffs maps VMAF 0..100 onto MOS 1..5.
type VMAFMapCoeffs struct {
	Kind VMAFMapKind `yaml:"kind"`
	W1   float64     `yaml:"w1"` // MOS = 1 + 4/(1+exp(-(V-W1)/W2))
	W2   float64     `yaml:"w2"`
}

// FuseCoeffs governs how the three no-reference loss sources are reconciled.
type FuseCoeffs struct {
	// KMax caps the CC wrap-correction factor. One full modulo-16 wrap is 16;
	// beyond that the sources disagree so violently that scaling is the wrong
	// response.
	KMax float64 `yaml:"k_max"`
	// DecoderFloorScale converts decoder errors per output frame into a floor
	// on Pplv, for when the decoder is visibly unhappy but the TS parser
	// measured zero loss. PLACEHOLDER: no principled derivation, it exists to
	// stop a silent-failure mode.
	DecoderFloorScale float64 `yaml:"decoder_floor_scale"`
	// TSPerSRTPktMax caps payload/188. 1316/188 = 7 is the usual value.
	TSPerSRTPktMax float64 `yaml:"ts_per_srt_pkt_max"`
}

// Correction is a monotone affine map applied to the finished score.
//
// This is the DEFAULT thing calibration fits, and deliberately so. Refitting
// all twelve video coefficients against noisy data overfits badly: V3/V4/V5
// trade off against V10..V12 almost freely, so many very different parameter
// sets explain the training data equally well and generalise worse than the
// shipped guesses. Two parameters cannot overfit, they fix the failure that
// actually dominates -- a systematic offset and slope against ground truth --
// and being monotone they cannot reorder anything.
type Correction struct {
	A float64 `yaml:"a"` // MOS' = clamp(A + B*MOS, 1, 5)
	B float64 `yaml:"b"`
}

// Identity reports whether the correction does nothing, which is the state of
// every shipped profile.
func (c Correction) Identity() bool { return c.A == 0 && (c.B == 0 || c.B == 1) }

// Apply maps a score through the correction. An unset correction is identity,
// so an uncalibrated profile behaves exactly as it did before.
func (c Correction) Apply(mos float64) float64 {
	if c.Identity() {
		return mos
	}
	return clamp(c.A+c.B*mos, 1, 5)
}

// Profile is a complete coefficient set.
type Profile struct {
	Name string `yaml:"name"`
	// Base names the shipped profile a fitted file layers onto, so a
	// calibration file carries only what it changed.
	Base       string     `yaml:"base,omitempty"`
	Provenance Provenance `yaml:"provenance"`

	Video    VideoCoeffs      `yaml:"video"`
	Audio    AudioCoeffs      `yaml:"audio"`
	Freeze   StallCoeffs      `yaml:"freeze"`
	AudioGap StallCoeffs      `yaml:"audio_gap"`
	Sync     SyncCoeffs       `yaml:"sync"`
	MM       MultimediaCoeffs `yaml:"mm"`
	VMAF     VMAFMapCoeffs    `yaml:"vmaf"`
	Fuse     FuseCoeffs       `yaml:"fuse"`

	// Correction is applied last, after every other term. Empty on the shipped
	// profiles; populated by `srtbench calibrate`.
	Correction Correction `yaml:"correction,omitempty"`
}
