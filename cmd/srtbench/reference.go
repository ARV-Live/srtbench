package main

import (
	"context"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/ARV-Live/srtbench/internal/config"
	"github.com/ARV-Live/srtbench/internal/qoe"
	"github.com/ARV-Live/srtbench/internal/refqual"
	"github.com/ARV-Live/srtbench/internal/sink"
)

// refRunner records short segments of the received stream on a duty cycle and
// scores them against a regenerated reference.
//
// It exists to give the cheap parametric model something to be calibrated
// against. The duty cycle is about leaving CPU headroom, not about VMAF being
// unable to keep up: measured here, libvmaf runs at 2.99x realtime at 1080p
// with 8 threads.
type refRunner struct {
	cfg  config.Config
	spec refqual.Spec
	out  sink.Sink
	prof qoe.Profile

	mu   sync.Mutex
	rec  *refqual.Recorder
	busy bool

	nextAt   time.Time
	stopAt   time.Time
	baseOnce sync.Once
	basePTS  float64
	haveBase bool
	verbose  bool
}

func newRefRunner(cfg config.Config, prof qoe.Profile, out sink.Sink, verbose bool) *refRunner {
	os.MkdirAll(refqual.TempDir(), 0o755)
	return &refRunner{
		cfg:  cfg,
		prof: prof,
		out:  out,
		spec: refqual.Spec{
			Input:     cfg.Media.Input,
			Width:     cfg.Media.Width,
			Height:    cfg.Media.Height,
			FPS:       cfg.Media.FPS,
			Threads:   cfg.QoE.VMAFThreads,
			WithAudio: !cfg.Media.NoAudio,
		},
		verbose: verbose,
		nextAt:  time.Now().Add(cfg.QoE.RefPeriod),
	}
}

// setBase records the stream's opening video timestamp, which anchors every
// later reference lookup.
func (r *refRunner) setBase(pts float64) {
	r.baseOnce.Do(func() { r.basePTS, r.haveBase = pts, true })
}

// feed is called from the read loop. It only ever appends to a bounded file,
// and never blocks: the reader must not be delayed by the reference pass.
func (r *refRunner) feed(b []byte) {
	r.mu.Lock()
	rec := r.rec
	r.mu.Unlock()
	if rec != nil {
		rec.Write(b)
	}
}

// tick advances the duty cycle once per scoring window.
func (r *refRunner) tick(ctx context.Context, now time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.rec == nil {
		if r.busy || now.Before(r.nextAt) || !r.haveBase {
			return
		}
		// Size the segment from the configured bitrate so the window is
		// bounded in bytes as well as in time.
		maxBytes := int64(r.cfg.QoE.RefWindow.Seconds()) *
			int64(r.cfg.Media.VideoKbps+r.cfg.Media.AudioKbps) * 1000 / 8
		if maxBytes < 1<<20 {
			maxBytes = 1 << 20
		}
		rec, err := refqual.NewRecorder(refqual.TempDir(), maxBytes*2)
		if err != nil {
			return
		}
		r.rec = rec
		r.stopAt = now.Add(r.cfg.QoE.RefWindow)
		return
	}

	if now.Before(r.stopAt) && !r.rec.Full() {
		return
	}
	rec := r.rec
	r.rec = nil
	r.busy = true
	r.nextAt = now.Add(r.cfg.QoE.RefPeriod)
	base := r.basePTS

	// Score off the hot path. A stalled ffmpeg here must never reach the SRT
	// reader.
	go func() {
		defer func() {
			rec.Remove()
			r.mu.Lock()
			r.busy = false
			r.mu.Unlock()
		}()
		r.score(ctx, rec.Path(), base, now)
	}()
}

func (r *refRunner) score(ctx context.Context, path string, base float64, at time.Time) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	res, err := refqual.Run(ctx, path, r.spec, base)
	if err != nil {
		if r.verbose {
			fmt.Fprintf(os.Stderr, "  [reference] %v\n", err)
		}
		return
	}
	model := qoe.NewModel(r.prof)
	mos := model.MOSFromVMAF(res.VMAF)

	fields := map[string]any{
		"vmaf":     res.VMAF,
		"mos_vmaf": mos,
		// An unaligned comparison still yields a plausible-looking score, so
		// this flag is what stops it being trusted or fitted against.
		"vmaf_aligned": res.Aligned,
	}
	if sdr, ok := refqual.RunAudio(ctx, path, r.spec, base); ok {
		// Audio SDR is a waveform difference and a weak perceptual proxy; it
		// is recorded as a change detector, never as a fitting target.
		fields["audio_sdr_db"] = sdr
	}

	// A separate measurement from the 1 Hz series, so the dense parametric
	// data is not littered with nulls that every dashboard query would then
	// have to fill().
	r.out.Write(sink.Point{
		Measurement: "srtbench_ref",
		Time:        at,
		Tags: map[string]string{
			"session_id": r.cfg.Session.ID,
			"host":       r.cfg.Session.Host,
			"profile":    r.prof.Name,
			"vmaf_model": res.Model,
		},
		Fields: fields,
	})
	if r.verbose {
		fmt.Fprintf(os.Stderr, "  [reference] VMAF %.2f -> MOS %.2f (aligned=%v)\n",
			res.VMAF, mos, res.Aligned)
	}
}
