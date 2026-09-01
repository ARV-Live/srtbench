package calib

import (
	"fmt"
	"math"
	"math/rand"
	"testing"

	"github.com/ARV-Live/srtbench/internal/qoe"
)

// synth builds pairs whose ground truth is a known affine function of the
// parametric score, so the fit has a right answer to be checked against.
func synth(cells, per int, a, b, noise float64, seed int64) Data {
	rng := rand.New(rand.NewSource(seed))
	var d Data
	for c := 0; c < cells; c++ {
		// Each cell sits at its own operating point, as a real sweep cell does.
		base := 1.5 + 3.0*float64(c)/float64(cells)
		for i := 0; i < per; i++ {
			p := base + rng.NormFloat64()*0.05
			d.Pairs = append(d.Pairs, Pair{
				Cell:    fmt.Sprintf("cell-%d", c),
				Param:   p,
				Ref:     a + b*p + rng.NormFloat64()*noise,
				BrKbps:  2000,
				Fr:      30,
				LossPct: 0,
			})
		}
	}
	return d
}

func TestNelderMeadFindsKnownMinimum(t *testing.T) {
	// Rosenbrock: the standard awkward case for a simplex.
	f := func(x []float64) float64 {
		a, b := 1-x[0], x[1]-x[0]*x[0]
		return a*a + 100*b*b
	}
	got, v := NelderMead(f, []float64{-1.2, 1}, []float64{0.5, 0.5}, 4000)
	if v > 1e-6 {
		t.Fatalf("did not converge: f=%g at %v", v, got)
	}
	for i, want := range []float64{1, 1} {
		if math.Abs(got[i]-want) > 1e-3 {
			t.Errorf("x[%d] = %.5f, want %.f", i, got[i], want)
		}
	}
}

func TestNelderMeadIsDeterministic(t *testing.T) {
	f := func(x []float64) float64 { return (x[0] - 3) * (x[0] - 3) }
	a, _ := NelderMead(f, []float64{0}, []float64{1}, 500)
	b, _ := NelderMead(f, []float64{0}, []float64{1}, 500)
	if a[0] != b[0] {
		t.Fatalf("same input gave different results: %v vs %v", a, b)
	}
}

// The bound transform must never let a search reach a value outside its range,
// because a non-positive scale parameter makes the model return NaN and the
// simplex then sits there forever calling it the best result.
func TestBoundsNeverEscape(t *testing.T) {
	b := Bound{Lo: 100, Hi: 4000}
	for _, u := range []float64{-1e6, -50, 0, 50, 1e6} {
		v := b.From(u)
		if v <= b.Lo-1e-6 || v >= b.Hi+1e-6 {
			t.Errorf("From(%g) = %g, outside [%g, %g]", u, v, b.Lo, b.Hi)
		}
	}
	for _, v := range []float64{100.001, 658, 3999} {
		if got := b.From(b.To(v)); math.Abs(got-v) > 1e-6*v {
			t.Errorf("round trip of %g gave %g", v, got)
		}
	}
}

func TestFitRecoversAKnownCorrection(t *testing.T) {
	// Ground truth is 0.6 + 0.85*param, a plausible "model is too harsh" shape.
	d := synth(8, 12, 0.6, 0.85, 0.02, 1)
	base, _ := qoe.LoadProfile("h264-1080p")

	r, err := Fit(d, base, DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	if !r.Accepted {
		t.Fatalf("a clean affine relationship was rejected: %s", r.Reason)
	}
	t.Logf("recovered MOS' = %.3f + %.3f*MOS (truth 0.600 + 0.850)", r.Correction.A, r.Correction.B)
	// The ridge biases slightly toward identity, so allow a little slack.
	if math.Abs(r.Correction.B-0.85) > 0.08 {
		t.Errorf("slope %.3f, want ~0.85", r.Correction.B)
	}
	if r.FitRMSE >= r.BaseRMSE {
		t.Errorf("held-out RMSE did not improve: %.3f -> %.3f", r.BaseRMSE, r.FitRMSE)
	}
}

// Data the model already predicts perfectly must be left alone, rather than
// having a correction fitted to its noise.
func TestFitDeclinesWhenNothingToImprove(t *testing.T) {
	d := synth(8, 12, 0, 1, 0.02, 7)
	base, _ := qoe.LoadProfile("h264-1080p")

	r, err := Fit(d, base, DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	if r.Accepted && math.Abs(r.Correction.B-1) > 0.06 {
		t.Errorf("fitted a needless correction: a=%.3f b=%.3f", r.Correction.A, r.Correction.B)
	}
	t.Logf("accepted=%v  %s", r.Accepted, r.Reason)
}

// The whole point of holding out by CELL is that windows within a cell are
// near-duplicates. If a cell contributes to both training and test, the
// reported error is leakage.
func TestCrossValidationHoldsOutWholeCells(t *testing.T) {
	d := synth(6, 10, 0.5, 0.9, 0.01, 3)
	cells := d.Cells()
	groups := folds(cells, 3)
	if len(groups) != 3 {
		t.Fatalf("want 3 folds, got %d", len(groups))
	}
	for _, held := range groups {
		train, test := split(d, held)
		trainCells := map[string]bool{}
		for _, p := range train {
			trainCells[p.Cell] = true
		}
		for _, p := range test {
			if trainCells[p.Cell] {
				t.Fatalf("cell %q appears in both train and test -- that is leakage", p.Cell)
			}
		}
	}
}

// Too few cells means there is nothing to hold out, and a fit would be
// indistinguishable from memorisation. That must be refused, not guessed at.
func TestFitRefusesTooFewCells(t *testing.T) {
	d := synth(2, 30, 0.5, 0.9, 0.01, 4)
	base, _ := qoe.LoadProfile("h264-1080p")
	if _, err := Fit(d, base, DefaultOptions()); err == nil {
		t.Fatal("fit accepted data with only 2 cells")
	}
}

// A negative slope would rank a good stream below a bad one. No residual
// justifies shipping that.
func TestNeverShipsAnInvertingCorrection(t *testing.T) {
	var d Data
	for c := 0; c < 6; c++ {
		for i := 0; i < 10; i++ {
			// Deliberately anti-correlated.
			p := 1 + float64(c)*0.6
			d.Pairs = append(d.Pairs, Pair{
				Cell: fmt.Sprintf("c%d", c), Param: p, Ref: 5 - p,
				BrKbps: 2000, Fr: 30,
			})
		}
	}
	corr := fitAffine(d.Pairs, DefaultOptions().Lambda)
	if corr.B <= 0 {
		t.Fatalf("shipped an inverting correction: b=%.3f", corr.B)
	}
}

// The ridge must dominate when there is little data, so a thin sweep degrades
// into "slightly adjusted defaults" rather than a confident new model.
func TestRidgeKeepsThinDataNearIdentity(t *testing.T) {
	thin := synth(3, 2, 2.0, 0.3, 0.4, 11)
	strong := fitAffine(thin.Pairs, 0)
	regular := fitAffine(thin.Pairs, 50)
	t.Logf("unregularised b=%.3f, regularised b=%.3f", strong.B, regular.B)
	if math.Abs(regular.B-1) >= math.Abs(strong.B-1) {
		t.Errorf("ridge did not pull toward identity: %.3f vs %.3f", regular.B, strong.B)
	}
}

func TestCorrectionIsIdentityByDefault(t *testing.T) {
	for name, p := range qoe.Profiles() {
		if !p.Correction.Identity() {
			t.Errorf("%s ships a non-identity correction", name)
		}
		if p.Provenance.Calibrated() {
			t.Errorf("%s claims to be calibrated", name)
		}
		if got := p.Correction.Apply(3.21); got != 3.21 {
			t.Errorf("%s: identity correction changed a score to %v", name, got)
		}
	}
}

func TestRankCorrelations(t *testing.T) {
	a := []float64{1, 2, 3, 4, 5}
	if got := Pearson(a, []float64{2, 4, 6, 8, 10}); math.Abs(got-1) > 1e-9 {
		t.Errorf("Pearson of a perfect linear relation = %v", got)
	}
	// Monotone but curved: Spearman should be 1 where Pearson is not.
	b := []float64{1, 4, 9, 16, 25}
	if got := Spearman(a, b); math.Abs(got-1) > 1e-9 {
		t.Errorf("Spearman of a monotone relation = %v", got)
	}
	if got := Pearson(a, b); got > 0.99 {
		t.Errorf("Pearson should be below 1 on a curved relation, got %v", got)
	}
}
