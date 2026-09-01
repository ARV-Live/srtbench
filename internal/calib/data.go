package calib

import (
	"encoding/csv"
	"fmt"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Pair is one training example: what the parametric model said, and what the
// full-reference metric said about the same stretch of stream.
type Pair struct {
	When  time.Time
	Cell  string // sweep cell this came from; the cross-validation unit
	Param float64
	Ref   float64
	VMAF  float64

	// Inputs kept so a block refit can re-run the model rather than only
	// correct its output.
	BrKbps  float64
	Fr      float64
	LossPct float64
}

// Data is a loaded training set.
type Data struct {
	Pairs []Pair
	// Skipped counts ref measurements that could not be used, and why. A fit
	// that silently trained on a third of the data it appeared to have is
	// worse than one that says so.
	SkippedUnaligned int
	SkippedNoOverlap int
	SkippedInvalid   int
}

// Cells lists the distinct sweep cells present.
func (d Data) Cells() []string {
	seen := map[string]bool{}
	var out []string
	for _, p := range d.Pairs {
		if !seen[p.Cell] {
			seen[p.Cell] = true
			out = append(out, p.Cell)
		}
	}
	sort.Strings(out)
	return out
}

type row struct {
	t    time.Time
	vals map[string]string
}

func readCSV(path string) ([]row, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	r := csv.NewReader(f)
	r.FieldsPerRecord = -1
	recs, err := r.ReadAll()
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if len(recs) < 2 {
		return nil, fmt.Errorf("%s: no data rows", path)
	}
	head := recs[0]
	out := make([]row, 0, len(recs)-1)
	for _, rec := range recs[1:] {
		m := make(map[string]string, len(head))
		for i, h := range head {
			if i < len(rec) {
				m[h] = rec[i]
			}
		}
		t, err := time.Parse(time.RFC3339Nano, m["time"])
		if err != nil {
			continue
		}
		out = append(out, row{t: t, vals: m})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].t.Before(out[j].t) })
	return out, nil
}

func num(m map[string]string, k string) (float64, bool) {
	s := strings.TrimSpace(m[k])
	if s == "" {
		return 0, false
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
		return 0, false
	}
	return v, true
}

// Load reads a parametric CSV and its companion reference CSV and pairs them.
//
// A VMAF score summarises a segment, not an instant, so each reference point is
// matched against the MEAN parametric score over the same span rather than
// against whichever 1 s window happens to share its timestamp. Comparing a
// segment metric to a single instant would inject the parametric series' own
// per-window noise straight into the fit.
func Load(qoePath, refPath string, refWindow time.Duration) (Data, error) {
	var d Data

	qoe, err := readCSV(qoePath)
	if err != nil {
		return d, err
	}
	ref, err := readCSV(refPath)
	if err != nil {
		return d, fmt.Errorf("reference data: %w (run a sweep with the reference "+
			"pass enabled; without ground truth there is nothing to calibrate against)", err)
	}

	for _, rr := range ref {
		// An unaligned VMAF comparison still returns a plausible number -- it
		// simply compared the wrong frames -- so it must never enter a fit.
		if strings.EqualFold(strings.TrimSpace(rr.vals["vmaf_aligned"]), "false") {
			d.SkippedUnaligned++
			continue
		}
		vmaf, ok := num(rr.vals, "vmaf")
		if !ok {
			d.SkippedInvalid++
			continue
		}
		refMOS, ok := num(rr.vals, "mos_vmaf")
		if !ok {
			d.SkippedInvalid++
			continue
		}

		lo := rr.t.Add(-refWindow)
		var (
			sum, br, fr, loss float64
			n                 int
			cell              string
		)
		for _, qr := range qoe {
			if qr.t.Before(lo) || qr.t.After(rr.t) {
				continue
			}
			if strings.EqualFold(qr.vals["valid"], "false") {
				continue
			}
			// Prefer the uncorrected score so a re-calibration fits the raw
			// model rather than compounding a correction on top of itself.
			v, ok := num(qr.vals, "mos_overall_uncorrected")
			if !ok {
				if v, ok = num(qr.vals, "mos_overall"); !ok {
					continue
				}
			}
			sum += v
			n++
			if x, ok := num(qr.vals, "br_kbps"); ok {
				br += x
			}
			if x, ok := num(qr.vals, "fr"); ok {
				fr += x
			}
			if x, ok := num(qr.vals, "effective_loss_pct"); ok {
				loss += x
			}
			if cell == "" {
				cell = qr.vals["tag_cell_id"]
			}
		}
		if n == 0 {
			d.SkippedNoOverlap++
			continue
		}
		if cell == "" {
			cell = "default"
		}
		f := float64(n)
		d.Pairs = append(d.Pairs, Pair{
			When: rr.t, Cell: cell,
			Param: sum / f, Ref: refMOS, VMAF: vmaf,
			BrKbps: br / f, Fr: fr / f, LossPct: loss / f,
		})
	}

	if len(d.Pairs) == 0 {
		return d, fmt.Errorf(
			"no usable (parametric, reference) pairs: %d unaligned, %d without "+
				"overlapping scored windows, %d unreadable",
			d.SkippedUnaligned, d.SkippedNoOverlap, d.SkippedInvalid)
	}
	return d, nil
}
