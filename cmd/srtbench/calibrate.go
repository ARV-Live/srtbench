package main

import (
	"fmt"
	"os"
	"time"

	"github.com/ARV-Live/srtbench/internal/calib"
	"github.com/ARV-Live/srtbench/internal/qoe"
)

// runCalibrate fits a profile against the VMAF ground truth in a sweep.
func runCalibrate(qoeCSV, refCSV, outPath, baseName string,
	refWindow time.Duration, opts calib.Options) error {

	if refCSV == "" {
		refCSV = refCSVPath(qoeCSV)
	}
	base, err := qoe.LoadProfile(baseName)
	if err != nil {
		return err
	}

	d, err := calib.Load(qoeCSV, refCSV, refWindow)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "srtbench: loaded %d pairs from %d cells (%s + %s)\n",
		len(d.Pairs), len(d.Cells()), qoeCSV, refCSV)
	if d.SkippedUnaligned+d.SkippedNoOverlap+d.SkippedInvalid > 0 {
		fmt.Fprintf(os.Stderr,
			"  skipped: %d unaligned, %d without overlapping scored windows, %d unreadable\n",
			d.SkippedUnaligned, d.SkippedNoOverlap, d.SkippedInvalid)
	}

	r, err := calib.Fit(d, base, opts)
	if err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr)
	fmt.Fprint(os.Stderr, r.Summary())

	if !r.Accepted {
		// Refusing is a real outcome, not a failure. A calibration that does not
		// generalise is worse than none, because it carries the authority of
		// having been "measured".
		fmt.Fprintf(os.Stderr, "\nsrtbench: not writing a profile -- %s\n", r.Reason)
		return nil
	}

	name := baseName + "-fitted"
	p := calib.Apply(base, r, name)
	if err := p.Validate(); err != nil {
		return fmt.Errorf("fitted profile is not usable: %w", err)
	}

	header := fmt.Sprintf(`srtbench fitted profile
Generated %s from %s

%s
Held-out RMSE %.3f MOS (uncalibrated %.3f), across %d sweep cells.

Only the correction%s carries OriginFitted. The audio and multimedia blocks
remain estimated: VMAF carries no audio information, and no full-reference
metric says how a viewer combines audio with video.

Use with:  srtbench receive -profile-file %s`,
		time.Now().UTC().Format(time.RFC3339), qoeCSV,
		r.Summary(), r.FitRMSE, r.BaseRMSE, r.Cells,
		map[bool]string{true: " and the video loss block", false: ""}[r.RefitVideo],
		outPath)

	if err := qoe.SaveProfile(outPath, p, header); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "\nsrtbench: wrote %s\n", outPath)
	fmt.Fprintf(os.Stderr, "  use it with:  srtbench receive -profile-file %s\n", outPath)
	return nil
}
