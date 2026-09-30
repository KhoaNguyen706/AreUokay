# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project

AreUokay detects falls in real time from phone motion data. A Go core ingests Sensor Logger pushes, cuts them into windows, gets scores from a Python worker and logs `FALL`. The code is at Phase 1 ("thin slice") of `Fall Detection Platform Design Doc and Game Plan.md`, which is the source of truth for the roadmap, the goals and what is out of scope. `CODEBASE_EXPLAINED.md` is a file-by-file walkthrough; its `file:line` references go stale when code moves. Both docs are gitignored, so they exist only in the owner's local copy.

Both halves use only the standard library: `go.mod` (Go 1.23, module `areuokay`) has no requirements, and the worker imports nothing outside Python's stdlib.

## Commands

```sh
go test ./...                                                # all Go tests
go test ./internal/window -run TestCutStridesBySampleTime    # one Go test
cd worker && python -m unittest                              # Python tests
cd worker && python -m unittest test_server.ScoreTest.test_standing_still_is_not   # one Python test

docker compose up -d --build     # core + worker; CORE_PORT=18080 if 8080 is taken
docker compose logs -f core      # FALL lines appear here
docker compose run --rm replay /testdata/synthetic/F00_SYNTH_fall.txt   # prints FALL
docker compose run --rm replay /testdata/synthetic/D00_SYNTH_sit.txt    # doesn't
```

On Git Bash, set `MSYS_NO_PATHCONV=1` before the `replay` commands, or the `/testdata/...` paths get rewritten.

Without Docker: `python worker/server.py` (port from `PORT`, default 8000), then `go run ./cmd/core -worker http://localhost:8000` (no `-worker` means the Go threshold rule only), then `go run ./cmd/replay [-speed 4] <file>`. Replay takes a SisFall `.txt`, a Sensor Logger CSV export folder, or one Sensor Logger CSV named after its sensor. Real SisFall files go in `./data/`, which is gitignored and mounted at `/data` in the replay container.

The root `Dockerfile` builds both `core` and `replay` into one image. The `replay` Compose service is in the `tools` profile, so `up` doesn't start it.

## Architecture

Data path: phone or `cmd/replay` → `POST /ingest?token=…` (`internal/ingest`) → per-device ring buffer (`internal/window`) → `detect()` in `cmd/core/main.go` → `infer.Scorer` → worker `POST /score`, or the Go threshold rule if that fails → `FALL` log line.

The core runs two concurrent parts that share `window.Registry`: the HTTP server, which adds samples, and the `detect()` goroutine, which every `window.Stride` (0.5 s) cuts windows from all devices and scores them in one batch. Each `Buffer` has its own lock, and `Registry.CutAll` releases the registry lock before cutting.

Cross-file rules that are easy to break:

- **Units.** On the wire to `/ingest`, values are in Sensor Logger units: m/s², rad/s and unix-nanosecond timestamps. `ingest` converts to g and deg/s, and everything after it (`window.Sample`, the threshold rule, the worker) uses g and deg/s. `cmd/replay` converts SisFall's raw counts to phone units on purpose, so a replay takes the same path as a live phone.
- **Stream merging.** Sensor Logger sends separate streams. `ingest.merger` makes one sample per acceleration reading and attaches the latest gyroscope reading (sample-and-hold). It prefers `totalacceleration`. The `accelerometer` stream excludes gravity, so the latest `gravity` reading is added back to it. Replay emits gyro before accel at the same timestamp so the held gyro value is current.
- **Sample time, not wall time.** Windows (`window.Length` 2 s, `window.Stride` 0.5 s) are cut by sample timestamps, so replay at any `-speed` gives the same windows. A window spanning less than 1.5 s (a gap in the data) is dropped, and a device more than 10 s behind skips ahead. Latency is measured from the arrival of the batch that held the window's last sample (`Window.Arrived`).
- **One impact lands in 4 overlapping windows.** That's why `detect()` has a 5 s per-device cooldown, measured in sample time.
- **Never silent.** `infer.Scorer.Score` always returns one score per window. It falls back to `infer.DefaultThreshold` if the worker is unset, down, slower than 300 ms, returns non-200, or returns the wrong number of scores. The `model=` field in the FALL line shows what scored the window: `threshold-py-v0` is the worker, `threshold-go-v0` is the Go fallback.
- **The threshold rule exists twice:** `infer.DefaultThreshold` in Go, and the constants plus `score()` in `worker/server.py`. Change both together. The Python tests mirror the Go test cases.
- **Worker contract.** `POST /score` takes `{"windows":[{"device_id","t","ax","ay","az","gx","gy","gz"}]}` (columns, not rows) and returns `{"scores":[...],"model_version":"..."}`. This mirrors the planned gRPC contract `Inference.Score(WindowBatch) → ScoreBatch`. Phase 2 replaces `score()` with the ONNX model (and HTTP with gRPC) without changing what Go sends.
- **Phase 1 limits.** The `token` query parameter is just the device ID. There's no auth, gRPC or batching yet; the design doc puts them in later phases.

## Working on this project

These come from the design doc's "Rules for staying on track":

- The owner writes the core themselves: batching, the alert engine and the training loop. For those, explain and review rather than write the code, unless asked. Throwaway parts, such as the web pages, are fine to generate.
- New ideas go on the design doc's v2 list, not into the code, until Phase 3 is done.
- Move to the next phase only when the current phase's "done when" check passes.
