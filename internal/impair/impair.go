// Package impair deliberately damages the send path.
//
// This is what makes the tool a benchmark rather than a monitor: you can plot
// a MOS degradation curve on demand instead of waiting for a bad uplink to
// happen. Because we own the SRT socket, it costs only a few lines.
package impair

import (
	"io"
	"math/rand"
	"time"
)

// Config describes the damage to inflict.
type Config struct {
	// LossPct is the percentage of writes to discard.
	LossPct float64
	// BurstLen drops packets in runs of this length rather than singly.
	//
	// It matters more than the loss rate for realism: mobile uplinks fail in
	// bursts, and burst loss is far more damaging than the same percentage
	// spread uniformly, because consecutive losses defeat interleaving and
	// wipe out whole slices. Uniform loss testing flatters a codec.
	BurstLen int
	// Delay adds constant latency; Jitter randomises it.
	Delay  time.Duration
	Jitter time.Duration
	Seed   int64
}

// Enabled reports whether any impairment is configured.
func (c Config) Enabled() bool {
	return c.LossPct > 0 || c.Delay > 0 || c.Jitter > 0
}

// Writer wraps a destination and applies the configured impairment.
type Writer struct {
	W   io.Writer
	cfg Config
	rng *rand.Rand

	// burstLeft counts how many further writes this burst still swallows.
	burstLeft int

	Dropped uint64
	Passed  uint64
}

// New wraps w. A fixed seed makes a sweep reproducible, which is the whole
// point of a benchmark: an unrepeatable degradation curve cannot be compared
// against last week's.
func New(w io.Writer, cfg Config) *Writer {
	seed := cfg.Seed
	if seed == 0 {
		seed = 1
	}
	return &Writer{W: w, cfg: cfg, rng: rand.New(rand.NewSource(seed))}
}

func (w *Writer) Write(b []byte) (int, error) {
	if w.cfg.Delay > 0 || w.cfg.Jitter > 0 {
		d := w.cfg.Delay
		if w.cfg.Jitter > 0 {
			d += time.Duration(w.rng.Int63n(int64(w.cfg.Jitter)))
		}
		time.Sleep(d)
	}

	if w.burstLeft > 0 {
		w.burstLeft--
		w.Dropped++
		// Report success: the caller must believe the write landed, exactly as
		// it would if the packet had been lost in the network.
		return len(b), nil
	}
	if w.cfg.LossPct > 0 {
		// A burst of length N starting with probability p yields a mean loss
		// rate of p*N, so the trigger probability is scaled down to keep the
		// configured percentage honest.
		n := w.cfg.BurstLen
		if n < 1 {
			n = 1
		}
		if w.rng.Float64()*100 < w.cfg.LossPct/float64(n) {
			w.burstLeft = n - 1
			w.Dropped++
			return len(b), nil
		}
	}
	w.Passed++
	return w.W.Write(b)
}
