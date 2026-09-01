package main

import (
	"fmt"
	"math"
	"os"
	"time"

	"github.com/ARV-Live/srtbench/internal/config"
	"github.com/ARV-Live/srtbench/internal/qoe"
	"github.com/ARV-Live/srtbench/internal/sink"
)

// State is the published quality band.
type State string

const (
	StateUnknown   State = "unknown"
	StateExcellent State = "excellent"
	StateGood      State = "good"
	StateFair      State = "fair"
	StatePoor      State = "poor"
	StateBad       State = "bad"
	// StateLost is structurally distinct from StateBad. "The stream is
	// terrible" and "there is no stream" are different operational facts and
	// must not share a bucket.
	StateLost State = "lost"
)

// smoother turns a noisy per-window score into a publishable state.
//
// The pipeline is median-of-3, then an asymmetric EWMA, then a hysteresis
// band. The asymmetry is the important part and is not a matter of taste:
// degradation is perceived instantly and completely, whereas one good second
// after a bad patch does not restore the experience. Symmetric smoothing gets
// both directions wrong at once -- too slow to report a problem, too quick to
// declare it over.
type smoother struct {
	recent   []float64
	ewma     float64
	haveEWMA bool
	state    State
	goodRun  int
}

func newSmoother() *smoother { return &smoother{state: StateUnknown} }

const (
	attackAlpha  = 0.50 // falling: react fast
	releaseAlpha = 0.12 // rising: recover slowly
	// Three consecutive better windows to improve, one to degrade.
	improveDwell = 3
)

// push returns the published state and the smoothed MOS. The RAW score is what
// gets stored for calibration; smoothing is presentation only, which is why
// Evaluate is pure and this lives outside it.
func (s *smoother) push(sc qoe.Score) (State, float64) {
	if !sc.Valid {
		// A discontinuity means the old value describes a different stream.
		s.recent, s.haveEWMA, s.goodRun = nil, false, 0
		if sc.Reason == qoe.ReasonNoConnection || sc.Reason == qoe.ReasonConnectionLost {
			s.state = StateLost
		} else {
			s.state = StateUnknown
		}
		return s.state, math.NaN()
	}

	// Median-of-3 rejects the single-window spike that a desynced parser can
	// produce. An EWMA would smear that spike across the next ten seconds.
	s.recent = append(s.recent, sc.MOSOverall)
	if len(s.recent) > 3 {
		s.recent = s.recent[1:]
	}
	m := median3(s.recent)

	// Smooth the headroom rather than the score, so the 1.0 floor is preserved
	// structurally instead of by clamping.
	h := m - 1
	if !s.haveEWMA {
		s.ewma, s.haveEWMA = h, true
	} else {
		a := releaseAlpha
		if h < s.ewma {
			a = attackAlpha
		}
		s.ewma = a*h + (1-a)*s.ewma
	}

	want := band(s.ewma + 1)
	if rank(want) < rank(s.state) {
		// Improving: require sustained evidence.
		s.goodRun++
		if s.goodRun >= improveDwell {
			s.state, s.goodRun = want, 0
		}
	} else if rank(want) > rank(s.state) {
		s.state, s.goodRun = want, 0 // degrading: publish immediately
	} else {
		s.goodRun = 0
	}
	if s.state == StateUnknown {
		s.state = want
	}
	return s.state, s.ewma + 1
}

// cellOf labels the sweep cell. Outside a sweep every window shares one cell,
// which is exactly why calibration refuses to fit data with too few of them.
func cellOf(cfg config.Config) string {
	if cfg.Session.Cell != "" {
		return cfg.Session.Cell
	}
	return "default"
}

// band applies the hysteresis thresholds.
func band(v float64) State {
	switch {
	case v >= 4.20:
		return StateExcellent
	case v >= 3.40:
		return StateGood
	case v >= 2.60:
		return StateFair
	case v >= 1.80:
		return StatePoor
	}
	return StateBad
}

func rank(s State) int {
	switch s {
	case StateExcellent:
		return 1
	case StateGood:
		return 2
	case StateFair:
		return 3
	case StatePoor:
		return 4
	case StateBad:
		return 5
	case StateLost:
		return 6
	}
	return 0
}

func median3(v []float64) float64 {
	switch len(v) {
	case 0:
		return 0
	case 1:
		return v[0]
	case 2:
		return (v[0] + v[1]) / 2
	}
	a, b, c := v[0], v[1], v[2]
	return math.Max(math.Min(a, b), math.Min(math.Max(a, b), c))
}

// publish writes every measurement for one window.
func publish(out sink.Sink, cfg config.Config, prof qoe.Profile,
	now time.Time, s qoe.Sample, sc qoe.Score, state State, smoothed float64) {

	tags := map[string]string{
		"session_id": cfg.Session.ID,
		"host":       cfg.Session.Host,
		"role":       "receiver",
		"profile":    prof.Name,
		"stream_id":  cfg.SRT.StreamID,
		"state":      string(state),
		"cell_id":    cellOf(cfg),
	}
	w := func(m string, f map[string]any) {
		out.Write(sink.Point{Measurement: m, Time: now, Tags: tags, Fields: f})
	}

	w("srt_transport", map[string]any{
		"pkt_recv":            int64(s.Net.PktRecvDelta),
		"pkt_recv_loss":       int64(s.Net.PktRecvLossDelta),
		"pkt_recv_retrans":    int64(s.Net.PktRecvRetransDelta),
		"pkt_recv_drop":       int64(s.Net.PktRecvDropDelta),
		"pkt_recv_undecrypt":  int64(s.Net.PktRecvUndecryptDelta),
		"byte_recv":           int64(s.Net.ByteRecvDelta),
		"ms_rtt":              s.Net.MsRTT,
		"mbps_recv_rate":      s.Net.MbpsRecvRate,
		"mbps_link_capacity":  s.Net.MbpsLinkCapacity,
		"ms_recv_buf":         s.Net.MsRecvBuf,
		"ms_recv_tsbpd_delay": s.Net.MsRecvTsbPdDelay,
		"pkt_flight_size":     int64(s.Net.PktFlightSize),
		"avg_payload_bytes":   s.Net.AvgPayloadBytes,
	})

	w("ts_stream", map[string]any{
		"pkts_total":       int64(s.TS.PktsTotalDelta),
		"sync_loss_events": int64(s.TS.SyncLossEvents),
		"cc_errors_video":  int64(s.TS.Video.CCErrEvents),
		"cc_errors_audio":  int64(s.TS.Audio.CCErrEvents),
		"cc_lost_video":    int64(s.TS.Video.CCGapPkts),
		"cc_lost_audio":    int64(s.TS.Audio.CCGapPkts),
		"pkts_video":       int64(s.TS.Video.PktsDelta),
		"pkts_audio":       int64(s.TS.Audio.PktsDelta),
		"pcr_jitter_ms":    s.TS.PCRJitterMS,
		"video_pid":        int64(s.TS.Video.PID),
		"audio_pid":        int64(s.TS.Audio.PID),
	})

	w("media_video", map[string]any{
		"frames":         int64(s.Video.FramesOutDelta),
		"frames_duped":   int64(s.Video.FramesDupDelta),
		"frames_dropped": int64(s.Video.FramesDropDelta),
		"decode_errors":  int64(s.Video.DecodeErrDelta),
		"corrupt_frames": int64(s.Video.CorruptFrameDelta),
		"freeze_count":   int64(s.Video.FreezeEvents),
		"freeze_ms":      s.Video.FreezeMsTotal,
		"freeze_ms_max":  s.Video.FreezeMsMax,
	})

	// Audio fields are only written when the stream actually carries audio.
	// Emitting zeros for a video-only stream would drag every dashboard mean
	// down with a cause that is very hard to find later.
	if s.Stream.HasAudioTrack {
		w("media_audio", map[string]any{
			"decode_errors": int64(s.Audio.DecodeErrDelta),
			"silence_ms":    s.Audio.SilenceMsTotal,
			"gap_count":     int64(s.Audio.GapEvents),
			"gap_ms":        s.Audio.GapMsTotal,
		})
	}
	if s.Sync.Valid {
		w("av_sync", map[string]any{
			"drift_ms":     s.Sync.SkewMs,
			"drift_sample": int64(s.Sync.Samples),
		})
	}

	// Every qoe point carries the SAME key set, with NaN where a value does
	// not apply.
	//
	// This is not cosmetic. The CSV sink fixes its columns from the first row
	// it sees, and the first row of any run is a warmup window; emitting a
	// narrower map there would lock the header without the MOS columns and
	// silently drop the primary measurement from the entire file. The Influx
	// sink discards NaN, so an inapplicable field is absent there rather than
	// misleadingly zero -- which is exactly what mos_audio needs when the
	// stream carries no audio track.
	nan := math.NaN()
	f := map[string]any{
		"valid":          sc.Valid,
		"reason":         string(sc.Reason),
		"confidence":     sc.Confidence,
		"loss_agreement": sc.LossAgreement,
		"extrapolated":   sc.Extrapolated,
		// Provenance travels with every point, so a dashboard can never
		// silently present an uncalibrated estimate as a measurement.
		"calibrated": prof.Provenance.Calibrated(),

		"mos_overall":             nan,
		"mos_overall_uncorrected": nan,
		"mos_overall_smoothed":    nan,
		"mos_video":               nan,
		"mos_audio":               nan,
		"effective_loss_pct":      nan,
		"audio_loss_pct":          nan,
		"br_kbps":                 nan,
		"br_kbps_received":        nan,
		"fr":                      nan,
		"fr_delivered":            nan,
		"i_coding":                nan,
		"dpplv":                   nan,
		"k_freeze":                nan,
		"k_sync":                  nan,
		"k_audio_gap":             nan,
		"wrap_factor_k":           nan,
		"r_audio":                 nan,
		"ie_eff_audio":            nan,
	}
	if sc.Valid {
		// Both are stored. The raw series is what the calibrator must consume;
		// the smoothed one is what a dashboard should display.
		f["mos_overall"] = sc.MOSOverall
		f["mos_overall_smoothed"] = smoothed
		// The pre-correction score is stored so calibration always fits the raw
		// model. Without it a second calibration would fit a correction on top
		// of the first one and compound them.
		if sc.Corrected {
			f["mos_overall_uncorrected"] = sc.MOSUncorrected
		} else {
			f["mos_overall_uncorrected"] = sc.MOSOverall
		}
		f["effective_loss_pct"] = sc.PplvPct
		f["audio_loss_pct"] = sc.PplaPct
		f["br_kbps"] = sc.BrKbps
		f["br_kbps_received"] = sc.BrKbpsReceived
		f["fr"] = sc.Fr
		f["fr_delivered"] = sc.FrDelivered
		f["i_coding"] = sc.ICoding
		f["dpplv"] = sc.DPplV
		f["k_freeze"] = sc.KFreeze
		f["k_sync"] = sc.KSync
		f["wrap_factor_k"] = sc.WrapFactorK
		if sc.HasVideo {
			f["mos_video"] = sc.MOSVideo
		}
		// mos_audio stays NaN with no audio track, so it is absent from the
		// database rather than a zero that would drag every mean down.
		if sc.HasAudio {
			f["mos_audio"] = sc.MOSAudio
			f["r_audio"] = sc.RAudio
			f["ie_eff_audio"] = sc.IeEffAudio
			f["k_audio_gap"] = sc.KAudioGap
		}
	}
	w("qoe", f)
}

func printWindow(sc qoe.Score, s qoe.Sample, state State) {
	if !sc.Valid {
		fmt.Fprintf(os.Stderr, "  [%-9s] %s\n", state, sc.Reason)
		return
	}
	audio := "  --"
	if sc.HasAudio {
		audio = fmt.Sprintf("%.2f", sc.MOSAudio)
	}
	fmt.Fprintf(os.Stderr,
		"  [%-9s] MOS %.2f (v %.2f a %s)  loss v%.3f%% a%.3f%%  %.0fkbps %.0ffps  rtt %.1fms  drop %d\n",
		state, sc.MOSOverall, sc.MOSVideo, audio,
		sc.PplvPct, sc.PplaPct, sc.BrKbps, sc.FrDelivered,
		s.Net.MsRTT, s.Net.PktRecvDropDelta)
}
