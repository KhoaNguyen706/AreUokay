// Package infer scores windows: through the Python worker when it is up,
// and with the threshold rule in Go when it is not.
package infer

import (
	"math"
	"time"

	"areuokay/internal/window"
)

// Threshold is the baseline rule: a free-fall dip in acceleration magnitude
// followed shortly by an impact spike. It is also the production fallback.
type Threshold struct {
	FreeFallG float64       // magnitude below this counts as free fall
	ImpactG   float64       // magnitude above this counts as an impact
	MaxGap    time.Duration // impact must follow the dip within this time
}

// DefaultThreshold is a starting point; tune it on your Phase 0 plots.
var DefaultThreshold = Threshold{FreeFallG: 0.6, ImpactG: 2.5, MaxGap: time.Second}

const ThresholdVersion = "threshold-go-v0"

// Score returns 1 for a possible fall and 0 otherwise.
func (th Threshold) Score(samples []window.Sample) float64 {
	var dipAt int64
	dipped := false
	for _, s := range samples {
		m := math.Sqrt(s.Ax*s.Ax + s.Ay*s.Ay + s.Az*s.Az)
		if m < th.FreeFallG {
			dipAt, dipped = s.T, true
			continue
		}
		if dipped && m > th.ImpactG && s.T-dipAt <= int64(th.MaxGap) {
			return 1
		}
	}
	return 0
}
