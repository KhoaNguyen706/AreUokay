# AreUokay: fall detection (Phase 1: thin slice)

Phone or replay → Go core (ingest, ring buffer, 2 s windows every 0.5 s) → Python worker scores the windows → `FALL` in the core's log.
If the worker is down or slow (300 ms timeout), the core scores with the same threshold rule in Go.

```
cmd/core          Go service: POST /ingest?token=…, GET /healthz, detection loop
cmd/replay        Streams a SisFall .txt or Sensor Logger CSV export to /ingest at real speed
internal/ingest   Parses Sensor Logger pushes, merges accel + gyro into samples (g, deg/s)
internal/window   Per-device ring buffer (2048 samples) and windowing by sample time
internal/infer    Threshold rule + worker client with fallback
worker/           Python worker skeleton: POST /score (JSON now, gRPC later)
testdata/         Synthetic SisFall-format fall and sit-down clips
```

## Run

```sh
docker compose up -d --build              # CORE_PORT=18080 if 8080 is taken
docker compose logs -f core

# in another terminal
docker compose run --rm replay /testdata/synthetic/F00_SYNTH_fall.txt        # prints FALL
docker compose run --rm replay /testdata/synthetic/D00_SYNTH_sit.txt         # doesn't
docker compose run --rm replay /data/SisFall_dataset/SA01/F01_SA01_R01.txt  # put SisFall in ./data
docker compose run --rm replay /data/my-recording                           # Sensor Logger export folder
```

`replay -speed 4` plays faster. Windows are cut by sample time, so the result is the same.
On Git Bash, set `MSYS_NO_PATHCONV=1` first, or the `/testdata/...` paths get rewritten.

Tests: `go test ./...` and `cd worker && python -m unittest`.

## Live phone

1. Put the laptop and phone on the same Wi-Fi and find the laptop's LAN IP.
2. In Sensor Logger, turn on **Accelerometer**, **Gravity** and **Gyroscope** (Total Acceleration is used when it's there).
3. Logger → gear → Data Streaming → Enable HTTP Push → `http://<laptop-ip>:8080/ingest?token=myphone`.
4. Start recording. The core logs `new device myphone, sensors in first batch: [...]`.
5. Drop the phone onto a bed and watch for `FALL`.

## Phase 1 notes

- Units in: Sensor Logger sends m/s² and rad/s, so ingest converts to g and deg/s. Check this against your Phase 0 plot.
- The Sensor Logger `accelerometer` stream excludes gravity, so ingest adds the latest `gravity` reading back. It prefers `totalacceleration` when the phone sends it.
- Threshold: a magnitude dip below 0.6 g, then a spike above 2.5 g within 1 s. The values are in `infer.DefaultThreshold` and `worker/server.py`. Tune them on real SisFall files.
- Each fall prints once. A 5 s cooldown per device covers the four overlapping windows that contain the same impact.
- Logged latency is from when the batch arrived to when `FALL` printed.
- Not in Phase 1: auth, gRPC and batching. These come in later phases.
