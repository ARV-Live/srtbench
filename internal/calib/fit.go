package calib

import (
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/ARV-Live/srtbench/internal/qoe"
)

// Result reports what a calibration run concluded.
type Result struct {
	Correction qoe.Correction
	Video      qoe.VideoCoeffs
	RefitVideo bool

	N     int
	Cells int

	// Every figure below is HELD OUT. In-sample error on this data would look
	// excellent and mean nothing: windows inside one sweep cell share an
	// encoder state and a loss seed, so a model can memorise a cell.
	BaseRMSE, FitRMSE         float64
	BasePearson, FitPearson   float64
	BaseSpearman, FitSpearman float64

	Accepted bool
	Reason   string

	// AffineOnlyRMSE is the held-out error of the two-parameter fit, kept even
	// when a video refit was requested so the two can be compared. A refit that
	// loses to the affine fit out of sample is overfitting, and the operator
	// needs to be told rather than left to adopt it.
	AffineOnlyRMSE float64
	// AtBound names coefficients the search drove to the edge of their allowed
	// range -- the classic signature of a fit straining against data that does
	// not actually determine it.
	AtBound []string
}

// Options tune a fit.
type Options struct {
	// RefitVideo additionally refits the video loss-sensitivity block. Off by
	// default: two parameters cannot overfit, twelve very much can.
	RefitVideo bool
	// Lambda pulls the fit toward the shipped defaults. Nonzero by default so
	// a thin dataset degrades into "slightly adjusted defaults" rather than
	// into a confidently wrong new model.
	Lambda float64
	// MinCells is the fewest distinct sweep cells a fit will accept. Below
	// this there is nothing to hold out and no way to tell fitting from
	// memorising.
	MinCells int
}

// DefaultOptions are deliberately conservative.
func DefaultOptions() Options {
	return Options{Lambda: 2.0, MinCells: 3}
}

// folds splits cells into k groups. The unit is the CELL, never the window.
//
// Windows within a cell are strongly autocorrelated -- same bitrate, same
// encoder state, same impairment seed -- so holding out random windows leaves
// near-duplicates of every held-out point in the training set. That reports a
// spectacular error which is pure leakage, and it is the single easiest way to
// convince yourself a bad calibration is a good one.
func folds(cells []string, k int) [][]string {
	if k > len(cells) {
		k = len(cells)
	}
	if k < 2 {
		return nil
	}
	out := make([][]string, k)
	for i, c := range cells {
		out[i%k] = append(out[i%k], c)
	}
	return out
}

func split(d Data, holdout []string) (train, test []Pair) {
	held := map[string]bool{}
	for _, c := range holdout {
		held[c] = true
	}
	for _, p := range d.Pairs {
		if held[p.Cell] {
			test = append(test, p)
		} else {
			train = append(train, p)
		}
	}
	return train, test
}

// fitAffine solves the ridge-regularised least squares for MOS' = a + b*MOS.
//
// Closed form rather than iterative: it is exact, instant, and cannot land in a
// local minimum. The ridge pulls toward the identity (a=0, b=1), so with little
// data the answer stays near "change nothing".
func fitAffine(pairs []Pair, lambda float64) qoe.Correction {
	n := float64(len(pairs))
	if n == 0 {
		return qoe.Correction{A: 0, B: 1}
	}
	var sx, sy, sxx, sxy float64
	for _, p := range pairs {
		sx += p.Param
		sy += p.Ref
		sxx += p.Param * p.Param
		sxy += p.Param * p.Ref
	}
	// Scale the penalty by sample count so lambda means the same thing whether
	// the sweep produced 20 pairs or 2000.
	la := lambda * n / 100
	lb := lambda * n / 100

	// [ n+la      sx      ] [a]   [ sy       ]
	// [ sx        sxx+lb  ] [b] = [ sxy + lb ]   (the lb on the right pulls b->1)
	a11, a12 := n+la, sx
	a21, a22 := sx, sxx+lb
	b1, b2 := sy, sxy+lb

	det := a11*a22 - a12*a21
	if math.Abs(det) < 1e-12 {
		return qoe.Correction{A: 0, B: 1}
	}
	a := (b1*a22 - a12*b2) / det
	b := (a11*b2 - b1*a21) / det
	if math.IsNaN(a) || math.IsNaN(b) || b <= 0 {
		// A non-positive slope would invert the ordering, turning a good stream
		// into a bad score. Never ship that, whatever the residual says.
		return qoe.Correction{A: 0, B: 1}
	}
	return qoe.Correction{A: a, B: b}
}

// fitVideoBlock refits the loss-sensitivity coefficients and the bitrate scale.
//
// These four are chosen deliberately rather than refitting all twelve: V10..V12
// set how fast quality falls with residual loss, V4 sets the bitrate at which
// quality saturates, and the rest of the chain (frame-rate tolerance, the
// quality ceiling) is far better determined by its derivation than by a short
// sweep. Fewer parameters, each one identifiable.
func fitVideoBlock(pairs []Pair, base qoe.VideoCoeffs, lambda float64) (qoe.VideoCoeffs, []string) {
	if len(pairs) < 8 {
		return base, nil
	}
	// Bounds keep the search in physically meaningful territory and, more
	// importantly, stop the simplex reaching a non-positive scale where the
	// model returns NaN and the search then never escapes.
	bounds := []Bound{
		{Lo: base.V4 * 0.25, Hi: base.V4 * 4}, // V4  bitrate scale
		{Lo: 0.05, Hi: 4.0},                   // V10 loss floor
		{Lo: 0.0, Hi: 3.0},                    // V11 frame-rate term
		{Lo: 0.0, Hi: 3.0},                    // V12 low-bitrate tolerance
	}
	start := []float64{base.V4, base.V10, base.V11, base.V12}
	u := make([]float64, len(start))
	for i := range start {
		u[i] = bounds[i].To(start[i])
	}

	apply := func(x []float64) qoe.VideoCoeffs {
		c := base
		c.V4 = bounds[0].From(x[0])
		c.V10 = bounds[1].From(x[1])
		c.V11 = bounds[2].From(x[2])
		c.V12 = bounds[3].From(x[3])
		return c
	}

	obj := func(x []float64) float64 {
		c := apply(x)
		var s float64
		var n int
		for _, p := range pairs {
			got := qoe.VideoScore(c, p.BrKbps, p.Fr, p.LossPct)
			if math.IsNaN(got) {
				continue
			}
			d := got - p.Ref
			s += d * d
			n++
		}
		if n == 0 {
			return math.Inf(1)
		}
		mse := s / float64(n)
		// Ridge toward the shipped values, in relative terms so each parameter
		// is penalised on its own scale.
		reg := 0.0
		for i, def := range []float64{base.V4, base.V10, base.V11, base.V12} {
			if def != 0 {
				r := (bounds[i].From(x[i]) - def) / def
				reg += r * r
			}
		}
		return mse + lambda/100*reg
	}

	best, _ := NelderMead(obj, u, ones(len(u), 0.5), 0)
	fitted := apply(best)

	var atBound []string
	for i, got := range []float64{fitted.V4, fitted.V10, fitted.V11, fitted.V12} {
		span := bounds[i].Hi - bounds[i].Lo
		if got <= bounds[i].Lo+0.01*span || got >= bounds[i].Hi-0.01*span {
			atBound = append(atBound, []string{"V4", "V10", "V11", "V12"}[i])
		}
	}
	return fitted, atBound
}

func ones(n int, v float64) []float64 {
	s := make([]float64, n)
	for i := range s {
		s[i] = v
	}
	return s
}

// Fit calibrates against ground truth and reports whether the result is worth
// keeping.
func Fit(d Data, base qoe.Profile, o Options) (Result, error) {
	r := Result{N: len(d.Pairs)}
	cells := d.Cells()
	r.Cells = len(cells)

	if r.Cells < o.MinCells {
		return r, fmt.Errorf(
			"only %d distinct sweep cell(s) in the data; need at least %d to hold "+
				"any out.\nWithout separate cells there is no way to tell a fit from "+
				"memorisation -- run `srtbench sweep` to produce a varied dataset",
			r.Cells, o.MinCells)
	}

	k := 5
	if k > r.Cells {
		k = r.Cells
	}
	groups := folds(cells, k)

	// Held-out predictions, gathered fold by fold. The affine-only variant is
	// always evaluated, so a video refit can be compared against it rather than
	// only against doing nothing.
	var baseP, fitP, affP, want []float64
	for _, held := range groups {
		train, test := split(d, held)
		if len(train) == 0 || len(test) == 0 {
			continue
		}
		corr := fitAffine(train, o.Lambda)
		var vid qoe.VideoCoeffs
		if o.RefitVideo {
			vid, _ = fitVideoBlock(train, base.Video, o.Lambda)
		}
		for _, p := range test {
			want = append(want, p.Ref)
			baseP = append(baseP, p.Param)
			affP = append(affP, corr.Apply(p.Param))
			pred := p.Param
			if o.RefitVideo {
				if v := qoe.VideoScore(vid, p.BrKbps, p.Fr, p.LossPct); !math.IsNaN(v) {
					pred = v
				}
			}
			fitP = append(fitP, corr.Apply(pred))
		}
	}
	if len(want) == 0 {
		return r, fmt.Errorf("cross-validation produced no held-out predictions")
	}

	r.BaseRMSE, r.FitRMSE = RMSE(baseP, want), RMSE(fitP, want)
	r.AffineOnlyRMSE = RMSE(affP, want)
	r.BasePearson, r.FitPearson = Pearson(baseP, want), Pearson(fitP, want)
	r.BaseSpearman, r.FitSpearman = Spearman(baseP, want), Spearman(fitP, want)

	// Refuse to ship a fit that is worse out of sample than doing nothing.
	if r.FitRMSE >= r.BaseRMSE {
		r.Reason = fmt.Sprintf(
			"held-out RMSE did not improve (%.3f fitted vs %.3f uncalibrated); "+
				"keeping the shipped coefficients", r.FitRMSE, r.BaseRMSE)
		return r, nil
	}

	// Only now refit on everything, for the coefficients actually shipped.
	r.Correction = fitAffine(d.Pairs, o.Lambda)
	if o.RefitVideo {
		r.Video, r.AtBound = fitVideoBlock(d.Pairs, base.Video, o.Lambda)
		r.RefitVideo = true
	}
	r.Accepted = true
	r.Reason = fmt.Sprintf("held-out RMSE improved %.3f -> %.3f (%.0f%% better)",
		r.BaseRMSE, r.FitRMSE, 100*(1-r.FitRMSE/r.BaseRMSE))
	return r, nil
}

// Apply writes an accepted fit into a profile, with honest provenance.
func Apply(base qoe.Profile, r Result, name string) qoe.Profile {
	p := base
	p.Base = base.Name
	p.Name = name
	p.Correction = r.Correction
	p.Provenance.Correction = qoe.OriginFitted
	if r.RefitVideo {
		p.Video = r.Video
		p.Provenance.Video = qoe.OriginFitted
	}
	// The audio block is NOT marked fitted. VMAF says nothing about audio, and
	// claiming otherwise would be the exact dishonesty the Provenance type
	// exists to prevent. Fitting it needs an audio ground truth such as ViSQOL.
	//
	// The multimedia weights are likewise untouched and stay estimated: VMAF
	// and an audio metric give video and audio truth SEPARATELY and say nothing
	// about how a viewer combines them. That needs subjective data.
	p.Provenance.FittedN = r.N
	p.Provenance.FittedCells = r.Cells
	p.Provenance.FittedRMSE = r.FitRMSE
	p.Provenance.Notes = fmt.Sprintf(
		"Fitted against VMAF ground truth from %d pairs across %d sweep cells. %s. "+
			"Audio and multimedia blocks remain estimated: VMAF carries no audio "+
			"information, and nothing here informs how the two combine.",
		r.N, r.Cells, r.Reason)
	return p
}

// Summary renders a result for the terminal.
func (r Result) Summary() string {
	s := fmt.Sprintf("  pairs            %d across %d sweep cells\n", r.N, r.Cells)
	s += fmt.Sprintf("  held-out RMSE    %.3f -> %.3f MOS\n", r.BaseRMSE, r.FitRMSE)
	s += fmt.Sprintf("  Pearson  r       %.3f -> %.3f\n", r.BasePearson, r.FitPearson)
	s += fmt.Sprintf("  Spearman rho     %.3f -> %.3f", r.BaseSpearman, r.FitSpearman)
	if r.BaseSpearman > 0.9 {
		s += "   (ordering was already sound; the fit corrects the scale)"
	}
	s += "\n"
	if r.Accepted {
		s += fmt.Sprintf("  correction       MOS' = %.4f + %.4f * MOS\n",
			r.Correction.A, r.Correction.B)
	}
	// A video refit that loses to the two-parameter fit out of sample is
	// overfitting by definition: more parameters explained the training cells
	// better and the held-out ones worse. Say so rather than let it be adopted.
	if r.RefitVideo && r.AffineOnlyRMSE < r.FitRMSE {
		s += fmt.Sprintf(
			"\n  WARNING: the video refit is WORSE out of sample than the two-parameter\n"+
				"  fit alone (%.3f vs %.3f held-out RMSE). That is overfitting.\n"+
				"  Re-run without -refit-video.\n",
			r.FitRMSE, r.AffineOnlyRMSE)
	}
	// A coefficient pinned to the edge of its range means the search kept
	// pushing and the data never pushed back -- the fit is straining against
	// something this sweep does not determine.
	if len(r.AtBound) > 0 {
		what := "it"
		if len(r.AtBound) > 1 {
			what = "them"
		}
		s += fmt.Sprintf(
			"\n  WARNING: %s reached the edge of the allowed range. This data does not\n"+
				"  determine %s; widen or lengthen the sweep, or drop -refit-video.\n",
			strings.Join(r.AtBound, ", "), what)
	}
	return s
}

// SortedCells is a stable cell list, for reporting.
func SortedCells(d Data) []string {
	c := d.Cells()
	sort.Strings(c)
	return c
}
