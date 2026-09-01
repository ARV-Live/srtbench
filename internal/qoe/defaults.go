package qoe

import "fmt"

// This file ships the default coefficient profiles.
//
// READ THIS BEFORE TRUSTING ANY NUMBER BELOW.
//
// There are ZERO OriginStandard video coefficients here. G.1070's published
// v1..v12 tables were fitted for MPEG-4/H.264 at QQVGA/QVGA, and transplanting
// QVGA MPEG-4 coefficients onto 1080p HEVC would be worse than a transparent
// guess: the bitrate-scale parameters are off by an order of magnitude for our
// operating point. So every video coefficient is OriginEstimated, derived from
// anchor points stated explicitly in the comments so you can redo or dispute
// the derivation. Do not cite them as G.1070 values.
//
// What we take from G.1070 is its STRUCTURE, which is the part that transfers.

// --- Video derivation -------------------------------------------------------
//
// At the optimal frame rate with zero loss the chain reduces to
//     MOS = 1 + V3 * x/(1+x)      where x = (Br/V4)^V5
// so two anchor points determine V4 and V5 once V3 is chosen.
//
// Anchors for HEVC 1080p30, zero loss (my judgement of HEVC rate-quality
// behaviour, NOT measurements):
//
//     6000 kbit/s -> 4.6      1500 -> 3.7      300 -> 2.0
//     3000 kbit/s -> 4.3       800 -> 3.0
//
// Take V3 = 3.9, i.e. an asymptotic ceiling of MOS 4.9 -- deliberately not
// 5.0, because no real encode is perceptually perfect. Then:
//
//     6000 -> x/(1+x) = 3.6/3.9 = 0.923 -> x = 12.0
//     3000 -> x/(1+x) = 3.3/3.9 = 0.846 -> x =  5.5
//     12.0/5.5 = 2.18 = 2^V5            -> V5 = 1.12
//     (6000/V4)^1.12 = 12.0             -> V4 = 658
//
// Verified against the three anchors NOT used in the fit: 1500 -> 3.79
// (target 3.7), 800 -> 3.16 (target 3.0), 300 -> 2.14 (target 2.0). The shape
// is right; the low-bitrate tail runs about 0.15 optimistic. That residual is
// exactly what a real sweep would remove.
//
// The other three profiles scale V4 ONLY, holding the shape (V3, V5) fixed.
// That shape-invariance is itself an assumption and is flagged as such:
//   - H.264 vs HEVC: x1.7, from HEVC's ~40-50% BD-rate advantage here.
//   - 720p vs 1080p: x0.55, from the ~2.25x pixel ratio, tempered because
//     bitrate demand scales sublinearly with pixel count.

// hevc1080p is the reference derivation; the others scale from it.
func videoHEVC1080p() VideoCoeffs {
	return VideoCoeffs{
		V1: 12.0, V2: 0.0060,
		V3: 3.90, V4: 658, V5: 1.12,
		V6: 0.95, V7: -3.0e-5,
		V8: 25, V9: 1200,
		V10: 0.25, V11: 0.35, V12: 0.55,
		OfrMaxFps: 60,
		BrMinKbps: 250, BrMaxKbps: 12000,
		FrMin: 10, FrMax: 60,
	}
}

func videoHEVC720p() VideoCoeffs {
	c := videoHEVC1080p()
	c.V2, c.V4, c.V9, c.V10 = 0.0090, 362, 700, 0.28
	return c
}

func videoH264_1080p() VideoCoeffs {
	c := videoHEVC1080p()
	// H.264 needs ~1.7x the bitrate for equivalent quality, and is somewhat
	// more loss-tolerant: HEVC's larger CTUs and stronger inter-prediction
	// make a lost slice propagate further. OriginEstimated, plausible but
	// unverified.
	c.V2, c.V4, c.V7 = 0.0035, 1120, -1.8e-5
	c.V9, c.V10, c.V11, c.V12 = 2000, 0.32, 0.40, 0.60
	return c
}

func videoH264_720p() VideoCoeffs {
	c := videoH264_1080p()
	c.V2, c.V4, c.V9, c.V10 = 0.0053, 615, 1200, 0.35
	return c
}

// --- Audio derivation -------------------------------------------------------
//
// Why NOT the narrowband E-model verbatim: G.107 was fitted for narrowband
// (300-3400 Hz) conversational speech through G.711/G.729/AMR. Applying it to
// 128 kbit/s stereo AAC-LC carrying music, ambience and speech from a
// bike-mounted mic is a category error on at least four counts -- bandwidth
// (everything is normalised to a 3.4 kHz ceiling), content (nobody is parsing
// phonemes out of background music), codec coverage (there is no narrowband Ie
// for AAC-LC, because the question does not arise), and delay (Id measures
// conversational difficulty, which is irrelevant one-way).
//
// So we keep the part that genuinely transfers -- the Ie,eff STRUCTURE, i.e.
// a codec/bitrate impairment plus a saturating loss impairment, combined on an
// impairment scale and mapped through a fixed nonlinearity -- and supply our
// own coefficients on the 0-100 R scale, treating R=100 as OUR ceiling rather
// than as narrowband-transparent speech. This is a deliberate, stated
// deviation.
//
// Anchors for AAC-LC stereo 48 kHz, as Ie on the impairment scale:
//     128 kbit/s -> Ie 4      96 -> Ie 7      64 -> Ie 15
// With IeFloor = 2 and IeCeil = 85 these give A2 = 2.9 and A1 = 36.
// Check at 96 -> Ie 6.6 (target 7). At 32 kbit/s it predicts Ie 50, harsher
// than a naive guess of 35 -- but 32 kbit/s stereo AAC-LC genuinely is bad, so
// the curve stands rather than being bent to taste.
//
// Bpl = 3 is deliberately LESS robust than G.711 without PLC (~4.3). An AAC-LC
// frame is 1024 samples (~21 ms), so losing one is an audible artifact rather
// than a sub-perceptual glitch; losing TS packets mid-frame also breaks the
// ADTS parse, so real damage often exceeds the nominal frame. There is no
// packet-loss concealment anywhere in the MPEG-TS path.

func audioAACLC() AudioCoeffs {
	return AudioCoeffs{
		R0:      100,
		IeFloor: 2, IeCeil: 85, A1: 36, A2: 2.9,
		Bpl: 3.0, IeMax: 95, Id: 0,
		RescaleTo5:   true,
		LossWindowMs: 5000,
		BrMinKbps:    24, BrMaxKbps: 320,
	}
}

func audioOpus() AudioCoeffs {
	c := audioAACLC()
	// Opus at 64 kbit/s is roughly AAC-LC at 96-128, and it has built-in PLC.
	// Lower confidence than the AAC row: Opus in MPEG-TS is unusual, and
	// AAC-LC is the realistic case for SRT contribution.
	c.A1, c.Bpl = 24, 6.0
	return c
}

// defaultProfile assembles the shared non-video blocks.
func defaultProfile(name string, v VideoCoeffs, a AudioCoeffs, vOrigin Origin) Profile {
	return Profile{
		Name: name,
		Provenance: Provenance{
			Video: vOrigin,
			Audio: OriginEstimated,
			// Freeze and audio-gap constants are pure structural guesses.
			// They are also the highest-value target for a calibration sweep,
			// because freezes dominate perceived quality on a real uplink.
			Stall: OriginPlaceholder,
			// The thresholds are standard (EBU R37, ITU-R BT.1359-1); how much
			// MOS a saturated lipsync error should cost is not.
			Sync: OriginStandard,
			MM:   OriginEstimated,
			VMAF: OriginEstimated,
			Notes: "Shipped defaults. Video coefficients are derived from stated " +
				"anchor points, not measured, and not G.1070 table values. " +
				"Run 'srtbench calibrate' before quoting these as absolute MOS.",
		},
		Video: v,
		Audio: a,
		Freeze: StallCoeffs{
			F1: 0.12, F2: 4.0, F3: 6000, F4: 0.25,
			MinStallMs: 150, KFloor: 0.05,
		},
		AudioGap: StallCoeffs{
			// Tighter than video freeze, because the ear has far finer
			// temporal resolution than the eye: a 100 ms video freeze often
			// passes unnoticed, a 100 ms audio dropout is an unmistakable click.
			F1: 0.06, F2: 3.0, F3: 6000, F4: 0.30,
			MinStallMs: 60, KFloor: 0.05,
		},
		Sync: SyncCoeffs{
			// The asymmetry is physical, not a fudge: sound never outruns
			// light, so audio arriving early has no natural analogue and the
			// ear flags it at once. Audio arriving late is what happens to
			// anyone at the back of a hall, and is tolerated 2-3x further.
			LeadOkMs: 40, LagOkMs: -60,
			LeadMaxMs: 90, LagMaxMs: -185,
			PenaltyMax: 0.45, MinSkewSamples: 10,
		},
		// Video weighted slightly above audio. Content-dependent -- for a
		// streamer whose value is commentary, audio should weigh more -- so
		// this is exposed as a config knob.
		MM: DeriveMM(0.45, 0.02),
		VMAF: VMAFMapCoeffs{
			Kind: VMAFMapLogistic,
			W1:   55, W2: 16,
		},
		Fuse: FuseCoeffs{
			KMax:              16, // one full modulo-16 CC wrap
			DecoderFloorScale: 0.5,
			TSPerSRTPktMax:    7, // 1316/188
		},
	}
}

// Profiles returns every shipped profile, keyed by name.
func Profiles() map[string]Profile {
	out := map[string]Profile{}
	for _, p := range []Profile{
		defaultProfile("h265-1080p", videoHEVC1080p(), audioAACLC(), OriginEstimated),
		defaultProfile("h265-720p", videoHEVC720p(), audioAACLC(), OriginEstimated),
		defaultProfile("h264-1080p", videoH264_1080p(), audioAACLC(), OriginEstimated),
		defaultProfile("h264-720p", videoH264_720p(), audioAACLC(), OriginEstimated),
	} {
		out[p.Name] = p
	}
	// Opus variants of the two most likely profiles.
	for _, base := range []string{"h264-1080p", "h265-1080p"} {
		p := out[base]
		p.Name = base + "-opus"
		p.Audio = audioOpus()
		out[p.Name] = p
	}
	return out
}

// DefaultProfileName is the safe starting point: H.264 is far cheaper to
// encode than HEVC, so the sender is less likely to become the bottleneck and
// skew the very measurement being taken.
const DefaultProfileName = "h264-1080p"

// LoadProfile returns a shipped profile by name.
func LoadProfile(name string) (Profile, error) {
	if name == "" {
		name = DefaultProfileName
	}
	p, ok := Profiles()[name]
	if !ok {
		return Profile{}, fmt.Errorf("unknown profile %q", name)
	}
	return p, nil
}
