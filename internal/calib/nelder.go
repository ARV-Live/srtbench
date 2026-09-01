// Package calib fits model coefficients against full-reference ground truth.
package calib

import (
	"math"
	"sort"
)

// Objective scores a parameter vector; lower is better.
type Objective func([]float64) float64

// NelderMead minimises f from a starting point by simplex search.
//
// Chosen over a gradient method because the objective runs the whole QoE model
// per sample and has no analytic derivative, and over Levenberg-Marquardt
// because at five parameters or fewer a simplex is entirely adequate and adds
// no numerical dependency. It is deterministic: the same data and start always
// produce the same fit, which matters because a calibration nobody can
// reproduce is not evidence of anything.
func NelderMead(f Objective, start, scale []float64, maxIter int) ([]float64, float64) {
	n := len(start)
	if n == 0 {
		return nil, math.Inf(1)
	}
	if maxIter <= 0 {
		maxIter = 400 * n
	}

	type vertex struct {
		x []float64
		v float64
	}
	pts := make([]vertex, n+1)
	pts[0] = vertex{append([]float64(nil), start...), f(start)}
	for i := 0; i < n; i++ {
		x := append([]float64(nil), start...)
		step := scale[i]
		if step == 0 {
			step = 0.1
		}
		x[i] += step
		pts[i+1] = vertex{x, f(x)}
	}

	const (
		alpha = 1.0 // reflection
		gamma = 2.0 // expansion
		rho   = 0.5 // contraction
		sigma = 0.5 // shrink
	)

	centroid := make([]float64, n)
	for iter := 0; iter < maxIter; iter++ {
		sort.Slice(pts, func(i, j int) bool { return pts[i].v < pts[j].v })

		// Converged once the simplex has collapsed in value terms.
		if math.Abs(pts[n].v-pts[0].v) <= 1e-10*(math.Abs(pts[0].v)+1e-10) {
			break
		}

		for i := range centroid {
			centroid[i] = 0
		}
		for _, p := range pts[:n] {
			for i, v := range p.x {
				centroid[i] += v / float64(n)
			}
		}

		reflect := make([]float64, n)
		for i := range reflect {
			reflect[i] = centroid[i] + alpha*(centroid[i]-pts[n].x[i])
		}
		rv := f(reflect)

		switch {
		case rv < pts[0].v:
			expand := make([]float64, n)
			for i := range expand {
				expand[i] = centroid[i] + gamma*(reflect[i]-centroid[i])
			}
			if ev := f(expand); ev < rv {
				pts[n] = vertex{expand, ev}
			} else {
				pts[n] = vertex{reflect, rv}
			}
		case rv < pts[n-1].v:
			pts[n] = vertex{reflect, rv}
		default:
			contract := make([]float64, n)
			for i := range contract {
				contract[i] = centroid[i] + rho*(pts[n].x[i]-centroid[i])
			}
			if cv := f(contract); cv < pts[n].v {
				pts[n] = vertex{contract, cv}
			} else {
				for k := 1; k <= n; k++ {
					x := make([]float64, n)
					for i := range x {
						x[i] = pts[0].x[i] + sigma*(pts[k].x[i]-pts[0].x[i])
					}
					pts[k] = vertex{x, f(x)}
				}
			}
		}
	}
	sort.Slice(pts, func(i, j int) bool { return pts[i].v < pts[j].v })
	return pts[0].x, pts[0].v
}

// Bound maps an unconstrained search variable onto [lo, hi].
//
// Constraints are enforced by transform rather than by rejecting bad vertices,
// because a simplex that can step onto a non-positive scale parameter finds
// NaN, and every comparison against NaN is false, so the search sits there
// forever reporting a "best" value that is not a number.
type Bound struct{ Lo, Hi float64 }

// To maps a real value into the unconstrained space.
func (b Bound) To(v float64) float64 {
	if b.Hi <= b.Lo {
		return v
	}
	t := (v - b.Lo) / (b.Hi - b.Lo)
	t = math.Min(math.Max(t, 1e-9), 1-1e-9)
	return math.Log(t / (1 - t))
}

// From maps an unconstrained value back into [Lo, Hi].
func (b Bound) From(u float64) float64 {
	if b.Hi <= b.Lo {
		return u
	}
	return b.Lo + (b.Hi-b.Lo)/(1+math.Exp(-u))
}

// RMSE is the root-mean-square error between paired values.
func RMSE(pred, want []float64) float64 {
	if len(pred) == 0 || len(pred) != len(want) {
		return math.Inf(1)
	}
	var s float64
	for i := range pred {
		d := pred[i] - want[i]
		s += d * d
	}
	return math.Sqrt(s / float64(len(pred)))
}

// Pearson and Spearman are both reported because they answer different
// questions: Pearson asks whether the numbers agree, Spearman whether the
// ORDERING agrees. A model that ranks streams correctly but is offset is
// fixable by the affine correction; one that ranks them wrongly is broken in a
// way no correction can repair, and only Spearman reveals that.
func Pearson(a, b []float64) float64 {
	n := len(a)
	if n < 2 || n != len(b) {
		return math.NaN()
	}
	var ma, mb float64
	for i := 0; i < n; i++ {
		ma += a[i] / float64(n)
		mb += b[i] / float64(n)
	}
	var num, da, db float64
	for i := 0; i < n; i++ {
		x, y := a[i]-ma, b[i]-mb
		num += x * y
		da += x * x
		db += y * y
	}
	if da == 0 || db == 0 {
		return math.NaN()
	}
	return num / math.Sqrt(da*db)
}

// Spearman is Pearson on ranks.
func Spearman(a, b []float64) float64 {
	return Pearson(ranks(a), ranks(b))
}

func ranks(v []float64) []float64 {
	n := len(v)
	idx := make([]int, n)
	for i := range idx {
		idx[i] = i
	}
	sort.SliceStable(idx, func(i, j int) bool { return v[idx[i]] < v[idx[j]] })
	r := make([]float64, n)
	for i := 0; i < n; {
		j := i
		for j+1 < n && v[idx[j+1]] == v[idx[i]] {
			j++
		}
		// Ties share the mean rank, or tied groups would bias the correlation.
		mean := float64(i+j)/2 + 1
		for k := i; k <= j; k++ {
			r[idx[k]] = mean
		}
		i = j + 1
	}
	return r
}
