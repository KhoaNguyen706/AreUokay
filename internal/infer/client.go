package infer

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"

	"areuokay/internal/window"
)

// Scorer sends windows to the Python worker and falls back to the threshold
// rule when the worker is unset, down, slow or returns garbage. It never
// returns without scores: silence is the worst failure for a safety alert.
//
// Phase 1 speaks plain JSON over HTTP. The shape mirrors the planned gRPC
// contract, Inference.Score(WindowBatch) -> ScoreBatch.
type Scorer struct {
	WorkerURL string // e.g. http://worker:8000; empty means threshold only
	Timeout   time.Duration
	Fallback  Threshold
	HTTP      *http.Client

	workerDown bool // only touched from the detection loop
}

type wireWindow struct {
	DeviceID string    `json:"device_id"`
	T        []int64   `json:"t"`
	Ax       []float64 `json:"ax"`
	Ay       []float64 `json:"ay"`
	Az       []float64 `json:"az"`
	Gx       []float64 `json:"gx"`
	Gy       []float64 `json:"gy"`
	Gz       []float64 `json:"gz"`
}

type windowBatch struct {
	Windows []wireWindow `json:"windows"`
}

type scoreBatch struct {
	Scores       []float64 `json:"scores"`
	ModelVersion string    `json:"model_version"`
}

// Score returns one score per window and the version of whatever scored them.
func (s *Scorer) Score(ctx context.Context, ws []window.Window) ([]float64, string) {
	if s.WorkerURL != "" {
		scores, version, err := s.remote(ctx, ws)
		if err == nil {
			if s.workerDown {
				log.Printf("worker back up, model=%s", version)
				s.workerDown = false
			}
			return scores, version
		}
		if !s.workerDown {
			log.Printf("worker unavailable, falling back to threshold rule: %v", err)
			s.workerDown = true
		}
	}
	scores := make([]float64, len(ws))
	for i, w := range ws {
		scores[i] = s.Fallback.Score(w.Samples)
	}
	return scores, ThresholdVersion
}

func (s *Scorer) remote(ctx context.Context, ws []window.Window) ([]float64, string, error) {
	batch := windowBatch{Windows: make([]wireWindow, len(ws))}
	for i, w := range ws {
		n := len(w.Samples)
		ww := wireWindow{DeviceID: w.DeviceID, T: make([]int64, n),
			Ax: make([]float64, n), Ay: make([]float64, n), Az: make([]float64, n),
			Gx: make([]float64, n), Gy: make([]float64, n), Gz: make([]float64, n)}
		for j, x := range w.Samples {
			ww.T[j], ww.Ax[j], ww.Ay[j], ww.Az[j], ww.Gx[j], ww.Gy[j], ww.Gz[j] = x.T, x.Ax, x.Ay, x.Az, x.Gx, x.Gy, x.Gz
		}
		batch.Windows[i] = ww
	}
	body, err := json.Marshal(batch)
	if err != nil {
		return nil, "", err
	}
	ctx, cancel := context.WithTimeout(ctx, s.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.WorkerURL+"/score", bytes.NewReader(body))
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.HTTP.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("worker status %s", resp.Status)
	}
	var out scoreBatch
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, "", err
	}
	if len(out.Scores) != len(ws) {
		return nil, "", fmt.Errorf("worker returned %d scores for %d windows", len(out.Scores), len(ws))
	}
	return out.Scores, out.ModelVersion, nil
}
