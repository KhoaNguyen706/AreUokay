// Package window keeps each device's recent readings in a ring buffer and
// cuts them into overlapping fixed-length windows.
package window

import (
	"sort"
	"sync"
	"time"
)

const (
	Length = 2 * time.Second
	Stride = 500 * time.Millisecond

	// bufCap holds a little over 10 s at SisFall's 200 Hz (and 20 s at 100 Hz).
	bufCap = 2048
	// minSpan rejects windows with a hole in them (e.g. the phone paused).
	minSpan = int64(Length * 3 / 4)
	// maxLag: if a device falls this far behind, skip ahead instead of catching up.
	maxLag = int64(10 * time.Second)
)

// Sample is one merged reading: acceleration in g, rotation in deg/s.
type Sample struct {
	T          int64 // unix nanoseconds
	Ax, Ay, Az float64
	Gx, Gy, Gz float64
}

type Window struct {
	DeviceID string
	Arrived  time.Time // when the batch holding the window's last sample arrived
	Samples  []Sample
}

func (w Window) End() int64 { return w.Samples[len(w.Samples)-1].T }

type arrival struct {
	lastT int64
	at    time.Time
}

// Buffer is one device's recent history.
type Buffer struct {
	mu       sync.Mutex
	ring     [bufCap]Sample
	head, n  int // head is the index of the oldest sample
	nextEnd  int64
	arrivals []arrival // last few batches, to measure latency honestly
}

// Add appends samples in time order. Samples not newer than what the buffer
// already holds are dropped (duplicates, out-of-order retries).
func (b *Buffer) Add(at time.Time, samples []Sample) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, s := range samples {
		if b.n > 0 && s.T <= b.newest() {
			continue
		}
		if b.n == 0 && b.nextEnd == 0 {
			b.nextEnd = s.T + int64(Length)
		}
		if b.n < bufCap {
			b.ring[(b.head+b.n)%bufCap] = s
			b.n++
		} else {
			b.ring[b.head] = s
			b.head = (b.head + 1) % bufCap
		}
	}
	if b.n > 0 {
		b.arrivals = append(b.arrivals, arrival{b.newest(), at})
		if len(b.arrivals) > 32 {
			b.arrivals = b.arrivals[1:]
		}
	}
}

func (b *Buffer) newest() int64 { return b.ring[(b.head+b.n-1)%bufCap].T }

// Cut returns every window whose end time the data has now passed, one per
// Stride of sample time. It uses sample time, not wall time, so replays at
// any speed produce the same windows.
func (b *Buffer) Cut(deviceID string) []Window {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.n == 0 {
		return nil
	}
	newest := b.newest()
	if newest-b.nextEnd > maxLag {
		b.nextEnd = newest
	}
	var out []Window
	for ; b.nextEnd <= newest; b.nextEnd += int64(Stride) {
		start := b.nextEnd - int64(Length)
		var ws []Sample
		for i := 0; i < b.n; i++ {
			s := b.ring[(b.head+i)%bufCap]
			if s.T > start && s.T <= b.nextEnd {
				ws = append(ws, s)
			}
		}
		if len(ws) < 2 || ws[len(ws)-1].T-ws[0].T < minSpan {
			continue
		}
		out = append(out, Window{DeviceID: deviceID, Arrived: b.arrivedBy(ws[len(ws)-1].T), Samples: ws})
	}
	return out
}

func (b *Buffer) arrivedBy(t int64) time.Time {
	i := sort.Search(len(b.arrivals), func(i int) bool { return b.arrivals[i].lastT >= t })
	if i == len(b.arrivals) {
		return time.Now()
	}
	return b.arrivals[i].at
}

// Registry maps device IDs to their buffers.
type Registry struct {
	mu      sync.Mutex
	devices map[string]*Buffer
}

func NewRegistry() *Registry { return &Registry{devices: map[string]*Buffer{}} }

func (r *Registry) Get(deviceID string) *Buffer {
	r.mu.Lock()
	defer r.mu.Unlock()
	b, ok := r.devices[deviceID]
	if !ok {
		b = &Buffer{}
		r.devices[deviceID] = b
	}
	return b
}

// CutAll cuts the pending windows of every device.
func (r *Registry) CutAll() []Window {
	r.mu.Lock()
	ids := make([]string, 0, len(r.devices))
	bufs := make([]*Buffer, 0, len(r.devices))
	for id, b := range r.devices {
		ids = append(ids, id)
		bufs = append(bufs, b)
	}
	r.mu.Unlock()
	var out []Window
	for i, b := range bufs {
		out = append(out, b.Cut(ids[i])...)
	}
	return out
}
