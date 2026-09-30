package infer

import (
	"testing"
	"time"

	"areuokay/internal/window"
)

// at100Hz builds samples 10 ms apart with the given magnitudes on the y axis.
func at100Hz(mags ...[]float64) []window.Sample {
	var out []window.Sample
	for _, run := range mags {
		for _, m := range run {
			out = append(out, window.Sample{T: int64(len(out)) * int64(10*time.Millisecond), Ay: m})
		}
	}
	return out
}

func repeat(v float64, n int) []float64 {
	s := make([]float64, n)
	for i := range s {
		s[i] = v
	}
	return s
}

func TestThreshold(t *testing.T) {
	cases := []struct {
		name    string
		samples []window.Sample
		want    float64
	}{
		{"dip then impact", at100Hz(repeat(1, 50), repeat(0.2, 30), repeat(4, 5), repeat(1, 115)), 1},
		{"standing still", at100Hz(repeat(1, 200)), 0},
		{"impact without dip", at100Hz(repeat(1, 100), repeat(4, 5), repeat(1, 95)), 0},
		{"impact too late after dip", at100Hz(repeat(0.2, 10), repeat(1, 150), repeat(4, 5), repeat(1, 35)), 0},
		{"sitting down hard", at100Hz(repeat(1, 80), repeat(0.8, 20), repeat(1.8, 10), repeat(1, 90)), 0},
	}
	for _, c := range cases {
		if got := DefaultThreshold.Score(c.samples); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}
