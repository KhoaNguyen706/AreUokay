package window

import (
	"testing"
	"time"
)

const ms = int64(time.Millisecond)

func samples(from, to, step int64) []Sample {
	var out []Sample
	for t := from; t <= to; t += step {
		out = append(out, Sample{T: t})
	}
	return out
}

func TestCutStridesBySampleTime(t *testing.T) {
	var b Buffer
	b.Add(time.Now(), samples(10*ms, 1000*ms, 10*ms))
	if ws := b.Cut("d"); len(ws) != 0 {
		t.Fatalf("got %d windows from 1 s of data, want 0", len(ws))
	}
	// 5 s of data at 100 Hz: windows end at 2.01, 2.51, … 4.51 s.
	b.Add(time.Now(), samples(1010*ms, 5000*ms, 10*ms))
	ws := b.Cut("d")
	if len(ws) != 6 {
		t.Fatalf("got %d windows, want 6", len(ws))
	}
	for i, w := range ws {
		wantEnd := 2010*ms + int64(i)*int64(Stride)
		if w.End() != wantEnd || w.Samples[0].T != wantEnd-int64(Length)+10*ms || len(w.Samples) != 200 {
			t.Errorf("window %d: end=%d first=%d n=%d", i, w.End(), w.Samples[0].T, len(w.Samples))
		}
	}
	if ws := b.Cut("d"); len(ws) != 0 {
		t.Fatalf("second cut returned %d windows, want 0", len(ws))
	}
}

func TestAddDropsOldAndDuplicateSamples(t *testing.T) {
	var b Buffer
	b.Add(time.Now(), samples(10*ms, 100*ms, 10*ms))
	b.Add(time.Now(), samples(50*ms, 150*ms, 10*ms))
	if b.n != 15 {
		t.Fatalf("n=%d, want 15", b.n)
	}
}

func TestRingOverwritesOldest(t *testing.T) {
	var b Buffer
	b.Add(time.Now(), samples(1, bufCap+10, 1))
	if b.n != bufCap || b.ring[b.head].T != 11 || b.newest() != bufCap+10 {
		t.Fatalf("n=%d oldest=%d newest=%d", b.n, b.ring[b.head].T, b.newest())
	}
}

func TestCutSkipsWindowsWithGaps(t *testing.T) {
	var b Buffer
	b.Add(time.Now(), samples(10*ms, 1000*ms, 10*ms))
	b.Add(time.Now(), samples(9000*ms, 11000*ms, 10*ms)) // 8 s pause
	for _, w := range b.Cut("d") {
		if span := w.End() - w.Samples[0].T; span < minSpan {
			t.Errorf("window ending %d spans only %d", w.End(), span)
		}
	}
}
