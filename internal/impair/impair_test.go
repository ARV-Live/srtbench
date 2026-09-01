package impair

import (
	"io"
	"math"
	"testing"
)

// The configured percentage must be what actually gets dropped, including when
// dropping in bursts. A burst of N starting with probability p yields a mean
// loss of p*N, so the trigger has to be scaled or the tool would silently
// inject several times the loss it reports -- and every degradation curve
// plotted with it would be wrong on the x axis.
func TestLossRateMatchesConfigAcrossBurstLengths(t *testing.T) {
	const n = 200000
	for _, burst := range []int{1, 3, 10} {
		for _, want := range []float64{1, 5, 20} {
			w := New(io.Discard, Config{LossPct: want, BurstLen: burst, Seed: 42})
			for i := 0; i < n; i++ {
				w.Write([]byte("x"))
			}
			got := 100 * float64(w.Dropped) / float64(w.Dropped+w.Passed)
			// Allow a relative tolerance; bursts make the variance larger.
			if math.Abs(got-want) > math.Max(0.5, want*0.15) {
				t.Errorf("burst=%d want %.1f%% loss, measured %.2f%%", burst, want, got)
			}
		}
	}
}

// Dropped writes must report success. The caller has to believe the write
// landed, exactly as it would if the packet were lost in the network.
func TestDroppedWritesReportSuccess(t *testing.T) {
	w := New(io.Discard, Config{LossPct: 100, Seed: 1})
	n, err := w.Write(make([]byte, 1316))
	if err != nil {
		t.Fatalf("dropped write returned an error: %v", err)
	}
	if n != 1316 {
		t.Fatalf("dropped write reported %d bytes, want 1316", n)
	}
	if w.Dropped == 0 {
		t.Fatal("100%% loss dropped nothing")
	}
}

// A fixed seed must give an identical drop pattern, or a degradation curve
// cannot be compared against last week's.
func TestSeedIsReproducible(t *testing.T) {
	run := func() uint64 {
		w := New(io.Discard, Config{LossPct: 7, BurstLen: 4, Seed: 99})
		for i := 0; i < 20000; i++ {
			w.Write([]byte("x"))
		}
		return w.Dropped
	}
	if a, b := run(), run(); a != b {
		t.Fatalf("same seed gave different results: %d vs %d", a, b)
	}
}

func TestDisabledByDefault(t *testing.T) {
	if (Config{}).Enabled() {
		t.Fatal("a zero Config should not impair anything")
	}
}
