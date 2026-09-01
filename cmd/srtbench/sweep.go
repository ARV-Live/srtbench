package main

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/ARV-Live/srtbench/internal/config"
	"github.com/ARV-Live/srtbench/internal/qoe"
	"github.com/ARV-Live/srtbench/internal/sink"
	"github.com/ARV-Live/srtbench/internal/srt"
)

// cell is one point in the sweep grid.
type cell struct {
	ID      string
	Kbps    int
	FPS     int
	LossPct float64
	Burst   int
	Latency time.Duration
}

// buildGrid expands the sweep axes.
//
// The grid sweeps the IMPAIRMENT, never a target loss figure. SRT recovers most
// of what is injected, so the residual loss the model actually consumes is an
// outcome, not an input -- you cannot ask for 0.5% residual, you can only cause
// packet loss and observe what survives. Latency is on the grid for the same
// reason: it is the knob that decides how much loss ARQ can repair before SRT
// abandons it, so it moves residual loss more than the loss rate does.
func buildGrid(kbps []int, fps []int, loss []float64, burst int, lat []time.Duration) []cell {
	var out []cell
	n := 0
	for _, b := range kbps {
		for _, f := range fps {
			for _, l := range loss {
				for _, d := range lat {
					out = append(out, cell{
						ID:   fmt.Sprintf("c%02d-%dk-%dfps-%.2gpct-%dms", n, b, f, l, d.Milliseconds()),
						Kbps: b, FPS: f, LossPct: l, Burst: burst, Latency: d,
					})
					n++
				}
			}
		}
	}
	return out
}

func parseInts(s string) ([]int, error) {
	var out []int
	for _, p := range strings.Split(s, ",") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		v, err := strconv.Atoi(p)
		if err != nil {
			return nil, fmt.Errorf("%q is not a number", p)
		}
		out = append(out, v)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("empty list")
	}
	return out, nil
}

func parseFloats(s string) ([]float64, error) {
	var out []float64
	for _, p := range strings.Split(s, ",") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		v, err := strconv.ParseFloat(p, 64)
		if err != nil {
			return nil, fmt.Errorf("%q is not a number", p)
		}
		out = append(out, v)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("empty list")
	}
	return out, nil
}

func parseDurations(s string) ([]time.Duration, error) {
	var out []time.Duration
	for _, p := range strings.Split(s, ",") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		d, err := time.ParseDuration(p)
		if err != nil {
			return nil, fmt.Errorf("%q is not a duration", p)
		}
		out = append(out, d)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("empty list")
	}
	return out, nil
}

// sweepOptions configures a run.
type sweepOptions struct {
	Cells    []cell
	CellDur  time.Duration
	BasePort int
	Verbose  bool
}

// runSweep walks the grid, writing every window to one output tagged by cell.
func runSweep(ctx context.Context, cfg config.Config, o sweepOptions) error {
	if cfg.Influx.CSV == "" && cfg.Influx.URL == "" {
		return fmt.Errorf("a sweep needs somewhere to write: pass -csv <file>")
	}
	if cfg.Influx.CSV == "-" {
		return fmt.Errorf("a sweep writes two series and cannot use stdout; " +
			"pass -csv <file> so the reference series gets its companion file")
	}

	// One sink for the whole sweep. Rebuilding it per cell would truncate the
	// file each time and leave only the final cell's data.
	out, err := buildSink(cfg)
	if err != nil {
		return err
	}
	defer out.Close()

	total := time.Duration(len(o.Cells)) * o.CellDur
	fmt.Fprintf(os.Stderr,
		"srtbench: sweeping %d cells x %s (about %s total)\n",
		len(o.Cells), o.CellDur, total.Round(time.Second))

	for i, c := range o.Cells {
		if ctx.Err() != nil {
			break
		}
		cellCfg := cfg
		cellCfg.Media.VideoKbps = c.Kbps
		cellCfg.Media.FPS = c.FPS
		cellCfg.Impair.LossPct = c.LossPct
		cellCfg.Impair.BurstLen = c.Burst
		// A fixed seed per cell keeps the whole sweep reproducible, so two runs
		// a week apart are actually comparable.
		cellCfg.Impair.Seed = int64(i + 1)
		cellCfg.SRT.Latency = c.Latency
		cellCfg.Session.Cell = c.ID
		// Each cell gets its own port so a lingering socket from the previous
		// cell cannot be mistaken for this one's.
		cellCfg.SRT.Endpoint = fmt.Sprintf("srt://127.0.0.1:%d", o.BasePort+i)

		// Ground truth at full duty. The duty cycle exists to protect CPU in
		// production; during calibration dense ground truth IS the point, and
		// starving the fit to save CPU on a calibration box is a false economy.
		cellCfg.QoE.Reference = true
		cellCfg.QoE.RefWindow = 4 * time.Second
		cellCfg.QoE.RefPeriod = 5 * time.Second

		fmt.Fprintf(os.Stderr, "\n[%d/%d] %s\n", i+1, len(o.Cells), c.ID)

		cellCtx, cancel := context.WithTimeout(ctx, o.CellDur)
		err := runSweepCell(cellCtx, cellCfg, o.Verbose, out)
		cancel()
		if err != nil && ctx.Err() == nil {
			// One bad cell should not throw away the rest of an hour's sweep.
			fmt.Fprintf(os.Stderr, "  cell failed: %v (continuing)\n", err)
		}
	}
	return nil
}

// runSweepCell runs one cell: a local listener with a sender pointed at it.
func runSweepCell(ctx context.Context, cfg config.Config, verbose bool, out sink.Sink) error {
	recvCfg := cfg
	recvCfg.SRT.Mode = string(srt.ModeListener)
	recvDone := make(chan error, 1)
	go func() { recvDone <- runReceiveSink(ctx, recvCfg, verbose, out) }()

	select {
	case <-ctx.Done():
		return <-recvDone
	case <-time.After(300 * time.Millisecond):
	}

	sendCfg := cfg
	sendCfg.SRT.Mode = string(srt.ModeCaller)
	sendDone := make(chan error, 1)
	go func() { sendDone <- runSend(ctx, sendCfg) }()

	select {
	case <-ctx.Done():
	case err := <-recvDone:
		return err
	case err := <-sendDone:
		if err != nil {
			return err
		}
	}
	select {
	case err := <-recvDone:
		return err
	case <-time.After(3 * time.Second):
	}
	return nil
}

// describeSweep prints the grid before it runs, because an hour is a long time
// to discover you swept the wrong thing.
func describeSweep(o sweepOptions, prof qoe.Profile) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "  profile     %s\n", prof.Name)
	fmt.Fprintf(&sb, "  cells       %d\n", len(o.Cells))
	fmt.Fprintf(&sb, "  per cell    %s\n", o.CellDur)
	fmt.Fprintf(&sb, "  total       ~%s\n",
		(time.Duration(len(o.Cells)) * o.CellDur).Round(time.Second))
	return sb.String()
}
