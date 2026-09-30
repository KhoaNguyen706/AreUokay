// Package ingest receives Sensor Logger HTTP pushes and feeds each device's
// ring buffer.
package ingest

import (
	"encoding/json"
	"log"
	"net/http"
	"sort"
	"strconv"
	"sync"
	"time"

	"areuokay/internal/window"
)

const (
	standardGravity = 9.80665 // m/s² per g
	degPerRad       = 180 / 3.141592653589793
	maxBody         = 1 << 20
)

// Message is Sensor Logger's HTTP push body.
type Message struct {
	MessageID int     `json:"messageId"`
	SessionID string  `json:"sessionId"`
	DeviceID  string  `json:"deviceId"`
	Payload   []Entry `json:"payload"`
}

// Entry is one reading. Sensor Logger sends acceleration in m/s², rotation
// in rad/s and time in unix nanoseconds.
type Entry struct {
	Name   string      `json:"name"`
	Time   json.Number `json:"time"`
	Values struct {
		X, Y, Z float64
	} `json:"values"`
}

// Handler serves POST /ingest?token=… . In Phase 1 the token is just the
// device ID; hashing and rate limits come in Phase 4.
type Handler struct {
	Registry *window.Registry

	mu      sync.Mutex
	mergers map[string]*merger
}

func NewHandler(r *window.Registry) *Handler {
	return &Handler{Registry: r, mergers: map[string]*merger{}}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	at := time.Now()
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	token := r.URL.Query().Get("token")
	if token == "" {
		http.Error(w, "missing token", http.StatusUnauthorized)
		return
	}
	var msg Message
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody)).Decode(&msg); err != nil {
		http.Error(w, "bad JSON: "+err.Error(), http.StatusBadRequest)
		return
	}

	h.mu.Lock()
	m, ok := h.mergers[token]
	if !ok {
		m = &merger{}
		h.mergers[token] = m
		log.Printf("new device %s, sensors in first batch: %v", token, sensorNames(msg.Payload))
	}
	h.mu.Unlock()

	h.Registry.Get(token).Add(at, m.merge(msg.Payload))
	w.WriteHeader(http.StatusOK)
}

// merger turns Sensor Logger's separate sensor streams into merged samples.
// Each acceleration reading becomes a sample, carrying the latest gyroscope
// reading (sample-and-hold). The "accelerometer" stream excludes gravity, so
// "totalacceleration" is preferred; failing that, the latest gravity reading
// is added back.
type merger struct {
	mu       sync.Mutex
	gyro     [3]float64 // deg/s
	gravity  [3]float64 // g
	hasGrav  bool
	hasTotal bool
}

func (m *merger) merge(entries []Entry) []window.Sample {
	type timed struct {
		t int64
		e *Entry
	}
	ts := make([]timed, 0, len(entries))
	for i := range entries {
		if t, ok := parseTime(entries[i].Time); ok {
			ts = append(ts, timed{t, &entries[i]})
		}
	}
	sort.SliceStable(ts, func(i, j int) bool { return ts[i].t < ts[j].t })

	m.mu.Lock()
	defer m.mu.Unlock()
	var out []window.Sample
	for _, x := range ts {
		v := x.e.Values
		switch x.e.Name {
		case "gyroscope":
			m.gyro = [3]float64{v.X * degPerRad, v.Y * degPerRad, v.Z * degPerRad}
		case "gravity":
			m.gravity = [3]float64{v.X / standardGravity, v.Y / standardGravity, v.Z / standardGravity}
			m.hasGrav = true
		case "totalacceleration":
			m.hasTotal = true
			out = append(out, m.sample(x.t, v.X/standardGravity, v.Y/standardGravity, v.Z/standardGravity))
		case "accelerometer":
			if m.hasTotal || !m.hasGrav {
				continue
			}
			out = append(out, m.sample(x.t,
				v.X/standardGravity+m.gravity[0], v.Y/standardGravity+m.gravity[1], v.Z/standardGravity+m.gravity[2]))
		}
	}
	return out
}

func (m *merger) sample(t int64, ax, ay, az float64) window.Sample {
	return window.Sample{T: t, Ax: ax, Ay: ay, Az: az, Gx: m.gyro[0], Gy: m.gyro[1], Gz: m.gyro[2]}
}

func parseTime(n json.Number) (int64, bool) {
	if t, err := strconv.ParseInt(string(n), 10, 64); err == nil {
		return t, true
	}
	f, err := strconv.ParseFloat(string(n), 64)
	return int64(f), err == nil
}

func sensorNames(entries []Entry) []string {
	seen := map[string]bool{}
	var names []string
	for _, e := range entries {
		if !seen[e.Name] {
			seen[e.Name] = true
			names = append(names, e.Name)
		}
	}
	return names
}
