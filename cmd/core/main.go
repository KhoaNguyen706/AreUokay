// Command core is the Go service: ingest, windowing and fall detection.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	"areuokay/internal/infer"
	"areuokay/internal/ingest"
	"areuokay/internal/window"
)

// cooldown stops one fall, seen in up to four overlapping windows, from
// printing FALL four times.
const cooldown = int64(5 * time.Second)

func main() {
	addr := flag.String("addr", envOr("ADDR", ":8080"), "listen address")
	workerURL := flag.String("worker", os.Getenv("WORKER_URL"), "Python worker base URL; empty = threshold rule only")
	threshold := flag.Float64("threshold", 0.5, "score at or above this is a possible fall")
	flag.Parse()

	reg := window.NewRegistry()
	scorer := &infer.Scorer{
		WorkerURL: *workerURL,
		Timeout:   300 * time.Millisecond,
		Fallback:  infer.DefaultThreshold,
		HTTP:      &http.Client{},
	}
	go detect(reg, scorer, *threshold)

	mux := http.NewServeMux()
	mux.Handle("/ingest", ingest.NewHandler(reg))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprintln(w, "ok") })
	log.Printf("core listening on %s, worker=%q", *addr, *workerURL)
	log.Fatal(http.ListenAndServe(*addr, mux))
}

// detect cuts windows every Stride, scores them and prints FALL.
func detect(reg *window.Registry, scorer *infer.Scorer, threshold float64) {
	lastFall := map[string]int64{}
	for range time.Tick(window.Stride) {
		ws := reg.CutAll()
		if len(ws) == 0 {
			continue
		}
		scores, version := scorer.Score(context.Background(), ws)
		for i, w := range ws {
			if scores[i] < threshold || w.End()-lastFall[w.DeviceID] < cooldown {
				continue
			}
			lastFall[w.DeviceID] = w.End()
			log.Printf("FALL device=%s score=%.2f model=%s window_end=%s latency=%s",
				w.DeviceID, scores[i], version,
				time.Unix(0, w.End()).Format("15:04:05.000"),
				time.Since(w.Arrived).Round(time.Millisecond))
		}
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
