# How the Codebase Works (Phase 1)

A walkthrough of every file, in the order data moves through the system.
For each block you get: **where it is** (file:line), **what goes in**, **what comes out**, and **why it's written that way**.

> Line numbers match the code as of Phase 1. If you edit a file, they'll shift a little — search for the function name instead.

---

## Table of contents

0. [The big picture](#0-the-big-picture)
1. [`cmd/replay/main.go` — the fake phone](#1-cmdreplaymaingo--the-fake-phone)
2. [`internal/ingest/ingest.go` — the front door](#2-internalingestingestgo--the-front-door)
3. [`internal/window/window.go` — memory and slicing](#3-internalwindowwindowgo--memory-and-slicing)
4. [`internal/infer/threshold.go` — the fall rule](#4-internalinferthresholdgo--the-fall-rule)
5. [`internal/infer/client.go` — talking to Python, with a safety net](#5-internalinferclientgo--talking-to-python-with-a-safety-net)
6. [`worker/server.py` — the Python side](#6-workerserverpy--the-python-side)
7. [`cmd/core/main.go` — wiring it all together](#7-cmdcoremaingo--wiring-it-all-together)
8. [Docker files](#8-docker-files)
9. [Tests — what each one proves](#9-tests--what-each-one-proves)
10. [Go concepts cheat sheet](#10-go-concepts-cheat-sheet)
11. [Exercises — how to actually learn this](#11-exercises--how-to-actually-learn-this)

---

## 0. The big picture

A phone falls. Here's the path its data takes:

```
Phone / replay tool                          cmd/replay/main.go
   │  every 1 s: JSON with ~100 readings (m/s², rad/s)
   ▼
POST /ingest?token=...                       internal/ingest/ingest.go
   │  converts units, merges accel + gyro into "Samples" (g, deg/s)
   ▼
per-device ring buffer                       internal/window/window.go
   │  keeps last ~10 s, cuts 2 s windows every 0.5 s
   ▼
detection loop (every 0.5 s)                 cmd/core/main.go  → detect()
   │  collects all new windows, sends them to be scored
   ▼
scorer                                       internal/infer/client.go
   │  → Python worker (worker/server.py)
   │  → if worker down: Go rule (internal/infer/threshold.go)
   ▼
score ≥ 0.5  →  log "FALL device=... latency=..."
```

Two things run **at the same time** inside the Go core:
- the **HTTP server** — accepts phone data and puts it in buffers
- the **detect loop** — every 0.5 s takes windows out of buffers and scores them

They share the buffers, which is why you'll see mutexes (locks) everywhere.

### File map

| File | Role | Language |
|---|---|---|
| `cmd/replay/main.go` | Fake phone: streams a recording to the server | Go |
| `internal/ingest/ingest.go` | Receives phone JSON, cleans it | Go |
| `internal/window/window.go` | Ring buffer + windowing | Go |
| `internal/infer/threshold.go` | Fall rule (baseline + fallback) | Go |
| `internal/infer/client.go` | Calls Python worker, falls back on failure | Go |
| `cmd/core/main.go` | Starts server + detect loop | Go |
| `worker/server.py` | Scoring service (model goes here in Phase 2) | Python |
| `Dockerfile`, `worker/Dockerfile`, `docker-compose.yml` | Packaging | Docker |
| `*_test.go`, `worker/test_server.py` | Tests | Go / Python |
| `testdata/synthetic/*.txt` | Fake fall + fake sit-down in SisFall format | data |

---

## 1. `cmd/replay/main.go` — the fake phone

**Job:** read a recording from disk and send it to the server *exactly* like a real phone would.

- **In:** a file path — SisFall `.txt`, a Sensor Logger CSV folder, or one Sensor Logger CSV file
- **Out:** one HTTP POST per second of data to `/ingest?token=replay`

### 1.1 SisFall conversion constants — `cmd/replay/main.go:32-37`

```go
sisfallAccelG   = 2.0 * 16 / (1 << 13)   // = 0.00390625 g per raw unit
sisfallGyroDegS = 2.0 * 2000 / (1 << 16) // ≈ 0.061 deg/s per raw unit
sisfallPeriod   = 5 ms                   // 200 Hz
```

SisFall stores **raw sensor counts**, not physical units. The accelerometer covers ±16 g (range 32 g) using 13 bits (8192 steps), so one count = 32/8192 g.

| Raw value | × 0.0039 | = |
|---|---|---|
| `-256` | | `-1.0 g` (gravity while standing) |
| `1024` | | `4.0 g` (hard impact) |

### 1.2 Message format structs — `cmd/replay/main.go:44-59`

`entry` and `message` describe the JSON the phone sends. Replay builds these and turns them into JSON. (Ingest has its own copy of these structs for reading — see 2.1.)

### 1.3 `main()` — the send loop — `cmd/replay/main.go:61-110`

**Step 1 — flags** (`:62-63`): `-url` (where to send) and `-speed` (1 = real time).

**Step 2 — load the file** (`:74`): calls `load()` → returns a list of readings sorted by time.

**Step 3 — rewrite timestamps to "now"** (`:82-83`)
```go
base := entries[0].Time
shift := time.Now().UnixNano() - base
```
SisFall timestamps start at 0; your Sensor Logger recording has timestamps from the day you recorded. `shift` is added to every timestamp so the recording looks like it's happening **right now**.

**Step 4 — send 1 second at a time** (`:89-107`)
```go
chunkEnd := entries[i].Time + int64(time.Second)          // :90
for j < len(entries) && entries[j].Time < chunkEnd { j++ } // :92-94  → entries[i:j] = 1 second of data
time.Sleep(time.Until(start.Add(...chunkEnd-base / speed))) // :96  → wait until that second has "happened"
post(...)                                                   // :102 → send it
```

Example with speed 1:
```
t=0.0s  wait 1 s  → send readings 0.0–1.0 s
t=1.0s  wait 1 s  → send readings 1.0–2.0 s
...
```
With `-speed 4`, each wait is 0.25 s instead. That's all "real speed" means.

### 1.4 `post()` — `cmd/replay/main.go:112-126`

Turns the message into JSON, POSTs it, returns an error if status isn't 200.

### 1.5 `load()` — pick the right parser — `cmd/replay/main.go:128-163`

| Input | What happens |
|---|---|
| a **folder** | reads every CSV whose name is `TotalAcceleration`, `Accelerometer`, `Gravity` or `Gyroscope` |
| a **`.csv` file** | reads that one file; sensor name comes from the file name |
| anything else | treated as a **SisFall `.txt`** |

Then sorts everything by time (`:161`).

`sensorName()` (`:165-168`): `"/data/rec/Gyroscope.csv"` → `"gyroscope"`.

### 1.6 `loadSisFall()` — `cmd/replay/main.go:172-202`

**In:** a line like `   17, -179,  -99,  -18, -504, -352,   76, -697, -279;`
**Out:** two entries: one `gyroscope`, one `totalacceleration`, same timestamp

For each line:
1. strip spaces and `;`, split by `,`
2. take the first 6 numbers (accel x,y,z + gyro x,y,z); ignore the 3rd sensor
3. time = line index × 5 ms
4. convert to **phone units**: raw → g → × 9.80665 → m/s²  (`:194`); raw → deg/s → rad/s

**Why convert back to phone units?** So the server only ever sees *one* input format. It can't tell replay from a real iPhone — so testing with replay really tests the live path.

```go
out = append(out, g, a) // :198 — gyro first, so the accel sample picks it up
```
Gyro goes *before* accel at the same timestamp. Why this matters: see sample-and-hold in 2.4.

### 1.7 `loadSensorLoggerCSV()` — `cmd/replay/main.go:206-244`

**In:** a CSV like
```
time,seconds_elapsed,z,y,x
1727600000000000000,0.0,-9.7,0.1,0.2
```
**Out:** list of entries with name = the sensor name.

- `:219-222` builds a column map from the header: `{"time":0, "seconds_elapsed":1, "z":2, "y":3, "x":4}`
- `:223-227` errors if `time`, `x`, `y`, or `z` is missing
- `:232-235` reads values **by column name, not position** → column order doesn't matter

---

## 2. `internal/ingest/ingest.go` — the front door

**Job:** receive the phone's JSON, check it, convert it into clean `Sample`s, hand them to the device's buffer.

**In** (what the phone sends):
```json
{"payload":[
  {"name":"gyroscope",         "time":1700000000010000000, "values":{"x":3.14,"y":0,"z":0}},
  {"name":"totalacceleration", "time":1700000000020000000, "values":{"x":0,"y":-9.8,"z":0}}
]}
```
**Out** (added to the buffer):
```go
Sample{T: 1700000000020000000, Ax: 0, Ay: -1.0, Az: 0, Gx: 180, Gy: 0, Gz: 0}
```
(−9.8 m/s² → −1 g; 3.14 rad/s → 180 deg/s)

### 2.1 Constants and structs — `internal/ingest/ingest.go:17-39`

```go
standardGravity = 9.80665   // m/s² in one g
degPerRad       = 180 / π
maxBody         = 1 << 20   // 1 MB
```

```go
type Entry struct {
    Name   string      `json:"name"`   // ← JSON tag: fill from key "name"
    Time   json.Number `json:"time"`
    Values struct{ X, Y, Z float64 } `json:"values"`
}
```
- The backtick text is a **JSON tag** — tells Go which JSON key maps to which field.
- `Time` is `json.Number` (not `float64`) because the timestamp has 19 digits. A `float64` only holds ~15–16 digits exactly, so nanoseconds would get rounded.

### 2.2 `Handler` struct — `internal/ingest/ingest.go:43-52`

```go
type Handler struct {
    Registry *window.Registry    // where device buffers live
    mu       sync.Mutex          // lock protecting `mergers`
    mergers  map[string]*merger  // one merger per device token
}
```

### 2.3 `ServeHTTP()` — runs once per request — `internal/ingest/ingest.go:54-82`

| Line | What | Why |
|---|---|---|
| `:55` | `at := time.Now()` | remember arrival time → used to measure latency later |
| `:56-59` | reject non-POST → 405 | |
| `:60-64` | read `?token=`, empty → 401 | in Phase 1 the token *is* the device ID |
| `:66-69` | decode JSON, max 1 MB → 400 if broken | `MaxBytesReader` stops a giant request from eating memory |
| `:71-78` | find or create this device's `merger` under a lock; log "new device" first time | |
| `:80` | `Registry.Get(token).Add(at, m.merge(msg.Payload))` | the actual work |
| `:81` | reply 200 | reply fast, the phone doesn't wait for detection |

**Read `:80` inside-out:**
1. `m.merge(msg.Payload)` → turn raw entries into Samples
2. `h.Registry.Get(token)` → find this device's buffer
3. `.Add(at, ...)` → store the samples

**Why the lock at `:71`?** Go handles each HTTP request in its own **goroutine** (lightweight thread). 50 phones = 50 goroutines running at once. If two write to the same `map` at the same time, Go crashes. `Lock()` / `Unlock()` = "one at a time in here."

### 2.4 `merger` + `merge()` — the trickiest part of ingest — `internal/ingest/ingest.go:89-133`

**Problem:** the phone sends **separate streams** — accel readings and gyro readings, each with its own timestamps. The model wants **one row with 6 numbers**.

**State it remembers** (`:89-95`):
```go
gyro     [3]float64 // latest gyro reading, deg/s
gravity  [3]float64 // latest gravity reading, g
hasGrav  bool       // have we seen gravity yet?
hasTotal bool       // does this phone send totalacceleration?
```

**Algorithm:**
1. `:102-107` parse each timestamp (skip broken ones)
2. `:108` sort entries by time
3. `:113-132` walk through in order:

| Sensor name | Line | Action | Creates a sample? |
|---|---|---|---|
| `gyroscope` | `:116` | remember it (rad → deg) | no |
| `gravity` | `:118` | remember it (m/s² → g) | no |
| `totalacceleration` | `:121` | convert to g, attach last gyro | **yes** |
| `accelerometer` | `:124` | add last gravity back, attach last gyro — *skipped* if phone sends `totalacceleration` or no gravity seen yet | **yes** (sometimes) |
| anything else (location, etc.) | — | ignored | no |

This technique is called **sample-and-hold**: each accel sample grabs "the most recent gyro value."

**Why add gravity back?** The iPhone's `accelerometer` stream *excludes gravity* — lying still it reads ~0. Our fall rule looks for "magnitude drops near 0 g" (free fall). Without gravity added back, a phone on a table would look like it's falling forever.

**Worked example:**
```
input (sorted):  gyro(t=10, x=π)  total(t=20, y=-9.8)  accel(t=30)  location(t=40)
                 │                 │                    │            │
                 remember 180°/s   → Sample{T:20,        skipped      ignored
                                     Ay:-1, Gx:180}      (hasTotal)
output: [ Sample{T:20, Ay:-1, Gx:180} ]
```
This exact case is tested in `internal/ingest/ingest_test.go:20`.

### 2.5 Small helpers — `internal/ingest/ingest.go:135-157`

- `sample()` (`:135`) — builds a `Sample` with the held gyro values
- `parseTime()` (`:139`) — timestamp as integer; if that fails, try decimal
- `sensorNames()` (`:147`) — list of unique sensor names, for the "new device" log line

---

## 3. `internal/window/window.go` — memory and slicing

**Job:** remember each device's recent readings, and cut them into **2-second windows, one every 0.5 seconds**.

- **In:** Samples from ingest
- **Out:** `Window{DeviceID, Arrived, Samples: [~200 samples]}`

### 3.1 Constants — `internal/window/window.go:11-22`

| Name | Value | Meaning |
|---|---|---|
| `Length` | 2 s | size of each window |
| `Stride` | 0.5 s | a new window every 0.5 s |
| `bufCap` | 2048 | max samples remembered per device (~10 s at 200 Hz) |
| `minSpan` | 1.5 s | window must cover ≥ 1.5 s, else it has a hole |
| `maxLag` | 10 s | if way behind, skip ahead instead of catching up |

### 3.2 Types — `internal/window/window.go:24-50`

- `Sample` (`:24`) — one reading: time `T` in unix nanoseconds, accel `Ax Ay Az` in g, gyro `Gx Gy Gz` in deg/s
- `Window` (`:30`) — device ID + when its data arrived + list of samples
- `End()` (`:36`) — timestamp of the last sample in the window
- `arrival` (`:38`) — "batch ending at sample time X arrived at wall-clock time Y"
- `Buffer` (`:44`) — one device's memory:

```go
ring     [2048]Sample   // fixed array — never grows
head, n  int            // head = index of oldest, n = how many stored
nextEnd  int64          // end time of the next window to cut
arrivals []arrival      // last 32 batches, for latency
```

### 3.3 The ring buffer — `Add()` — `internal/window/window.go:54-78`

A **ring buffer** is a fixed array where new items **overwrite the oldest** once it's full. Memory never grows.

Picture with capacity 4:
```
add 1,2,3,4:   [1 2 3 4]   head=0 (oldest=1), n=4
add 5:         [5 2 3 4]   head=1 (oldest=2)   ← 1 overwritten
add 6:         [5 6 3 4]   head=2 (oldest=3)
```

Line by line:

| Line | Code | Meaning |
|---|---|---|
| `:58` | `if b.n > 0 && s.T <= b.newest() { continue }` | drop samples not newer than what we have (duplicate / re-sent batch) |
| `:61-63` | `if b.n == 0 && b.nextEnd == 0 { b.nextEnd = s.T + Length }` | very first sample: first window ends 2 s later |
| `:64-66` | `ring[(head+n) % bufCap] = s; n++` | not full → write in next empty slot |
| `:67-69` | `ring[head] = s; head = (head+1) % bufCap` | full → overwrite oldest, move head forward |
| `:72-77` | append to `arrivals`, keep last 32 | for latency measurement |

`%` (modulo) makes the index wrap: `(2047 + 1) % 2048 = 0` → back to the start.

`newest()` (`:80`): the last stored sample is at index `(head + n - 1) % bufCap`.

### 3.4 `Cut()` — slice into windows — `internal/window/window.go:85-111`

```go
if newest-b.nextEnd > maxLag { b.nextEnd = newest }   // :92  way behind? jump ahead
for ; b.nextEnd <= newest; b.nextEnd += Stride {      // :96  every window whose end we've reached
    start := b.nextEnd - Length
    // :99-104  collect samples where start < T <= nextEnd
    // :105     skip if fewer than 2 samples or span < 1.5 s (gap in data)
    // :108     add the window
}
```

**Worked example** (exactly `internal/window/window_test.go:18`): buffer holds 100 Hz data from 0.01 s to 5.00 s.

```
window 1: (0.01, 2.01]     window 4: (1.51, 3.51]
window 2: (0.51, 2.51]     window 5: (2.01, 4.01]
window 3: (1.01, 3.01]     window 6: (2.51, 4.51]
```
6 windows × 200 samples. The next one would end at 5.01 — no data there yet, so it waits for the next batch. Calling `Cut()` again right away returns nothing (no duplicates).

**Key design decision: sample time, not wall time.** Windows are cut by the *phone's timestamps*, not the server clock. So replaying at 4× speed produces the exact same windows as real time.

**Windows overlap:** any moment in time is in **4 windows** (2 s ÷ 0.5 s). Remember this — it's why `main.go` needs a cooldown.

### 3.5 `arrivedBy()` — honest latency — `internal/window/window.go:113-119`

```go
i := sort.Search(len(b.arrivals), func(i int) bool { return b.arrivals[i].lastT >= t })
```
**Binary search**: find the first batch that contained the window's last sample. That batch's arrival time = the earliest moment the server *could* have known about this window. Latency = now − that.

### 3.6 `Registry` — all devices — `internal/window/window.go:122-155`

- `Registry` (`:122`) — `map[deviceID]*Buffer` + a lock
- `Get()` (`:129`) — returns the device's buffer, creating it if new
- `CutAll()` (`:141`) — cuts windows from every device:

```go
r.mu.Lock()
// copy the list of device IDs and buffers
r.mu.Unlock()
// cut each buffer — WITHOUT holding the registry lock
```
**Why copy then unlock?** If it held the registry lock while cutting 1,000 devices, every incoming phone request would be stuck waiting. Each `Buffer` has its own lock, so devices don't block each other.

---

## 4. `internal/infer/threshold.go` — the fall rule

**Job:** decide if one window looks like a fall.

- **In:** ~200 samples
- **Out:** `1` (possible fall) or `0`

### 4.1 Settings — `internal/infer/threshold.go:14-23`

```go
FreeFallG: 0.6   // magnitude below this = free fall
ImpactG:   2.5   // magnitude above this = impact
MaxGap:    1 s   // impact must come within 1 s after the dip
```
`ThresholdVersion = "threshold-go-v0"` — shows up in the FALL log as `model=` so you know who scored it.

### 4.2 `Score()` — `internal/infer/threshold.go:26-39`

```go
m := math.Sqrt(s.Ax*s.Ax + s.Ay*s.Ay + s.Az*s.Az)   // :30
```
**Magnitude** = total acceleration ignoring direction. You don't know how the phone sits in a pocket — magnitude is the same either way.

What a fall looks like in magnitude:
```
standing   ~1.0 g    (just gravity)
falling    ~0.15 g   ← free fall: body + phone drop together, feels "weightless"
impact     ~3.9 g    ← hitting the floor
lying      ~1.0 g
```

```go
if m < th.FreeFallG { dipAt, dipped = s.T, true; continue }                  // :31-34  saw a dip → remember when
if dipped && m > th.ImpactG && s.T-dipAt <= int64(th.MaxGap) { return 1 }  // :35-37  spike within 1 s of dip → FALL
return 0
```

| Input magnitudes | Result | Why |
|---|---|---|
| 1.0 … 0.2 … 4.0 | **1** | dip, then spike |
| 1.0 constant | 0 | no dip |
| 1.0 … 4.0 | 0 | spike without dip (bumped a table) |
| 0.2 … (1.5 s of 1.0) … 4.0 | 0 | spike too late after dip |
| 0.8 … 1.8 | 0 | hard sit-down: small dip, small spike |

This rule is the **baseline** your Phase 2 CNN must beat, and it's also the **fallback** when Python is down.

---

## 5. `internal/infer/client.go` — talking to Python, with a safety net

**Job:** get scores for windows from the Python worker; if that fails for any reason, use the Go rule.

- **In:** list of windows
- **Out:** list of scores (same length) + version string of whoever scored them

### 5.1 `Scorer` struct — `internal/infer/client.go:21-28`

```go
WorkerURL  string        // "http://worker:8000", or "" = Go rule only
Timeout    time.Duration // 300 ms
Fallback   Threshold     // the Go rule
HTTP       *http.Client
workerDown bool          // only used so we log "worker down" ONCE, not every 0.5 s
```

### 5.2 Wire format structs — `internal/infer/client.go:30-48`

What goes over the network to Python:
```json
{"windows":[{"device_id":"replay","t":[...],"ax":[...],"ay":[...],"az":[...],"gx":[...],"gy":[...],"gz":[...]}]}
```
and back:
```json
{"scores":[1.0], "model_version":"threshold-py-v0"}
```

### 5.3 `Score()` — the fallback logic — `internal/infer/client.go:51-71`

```
if WorkerURL is set:                             :52
    try remote()                                 :53
    success → (log "back up" if it was down)     :54-59
              return worker's scores
    failure → log "unavailable" once             :61-64
// worker unset OR failed:
score every window with the Go rule              :66-70
```
**It never returns without scores.** That's the design doc's "never go silent" goal — silence is the worst failure for a safety alert.

### 5.4 `remote()` — the HTTP call — `internal/infer/client.go:73-112`

| Line | What |
|---|---|
| `:74-84` | convert windows from **rows** to **columns** |
| `:85-88` | turn into JSON |
| `:89-90` | `context.WithTimeout(300 ms)` + `defer cancel()` |
| `:91-99` | build + send POST to `/score` |
| `:101-103` | status not 200 → error |
| `:105-107` | JSON doesn't parse → error |
| `:108-110` | number of scores ≠ number of windows → error |

**Rows → columns:**
```
Go has:  [{T:1, Ax:0.1, ...}, {T:2, Ax:0.2, ...}, ...]          (rows)
sends:   {"t":[1,2,...], "ax":[0.1,0.2,...], ...}               (columns)
```
Columns = smaller JSON, and it's the shape NumPy/PyTorch want anyway.

**`defer cancel()`** = "run this when the function exits, no matter how it exits." Frees the timer.

Any error in `remote()` → `Score()` falls back to the Go rule.

---

## 6. `worker/server.py` — the Python side

**Job:** score windows. Right now it uses the same rule as Go; in Phase 2 the CNN goes here.

- **In** (POST `/score`): the JSON from 5.2
- **Out:** `{"scores": [...], "model_version": "threshold-py-v0"}`

### 6.1 Settings — `worker/server.py:17-20`

Same numbers as Go: 0.6 g, 2.5 g, 1 s (in nanoseconds: `1_000_000_000`).

### 6.2 `score(w)` — `worker/server.py:23-32`

Line-for-line the same rule as `threshold.go`.
```python
for t, x, y, z in zip(w["t"], w["ax"], w["ay"], w["az"]):
```
`zip` walks the four column lists together — one sample at a time. Returns `1.0` (`:31`) or `0.0` (`:32`).

### 6.3 `Handler` — `worker/server.py:35-61`

| Method | Line | What |
|---|---|---|
| `do_GET` | `:36` | `/healthz` → `{"status":"ok"}`, else 404 |
| `do_POST` | `:42` | `/score` → read body, score each window, reply. Broken JSON → 400 (doesn't crash) |
| `_reply` | `:52` | helper: send status + JSON |
| `log_message` | `:60` | silenced — otherwise 2 log lines every second |

### 6.4 Startup — `worker/server.py:64-67`

Reads `PORT` (default 8000), starts a `ThreadingHTTPServer` (one thread per request).

**Why duplicate the Go rule in Python?** It's a **placeholder**. In Phase 2 you replace only `score()` with the ONNX model. The Go side keeps sending the same JSON and never changes.

---

## 7. `cmd/core/main.go` — wiring it all together

### 7.1 `cooldown` — `cmd/core/main.go:18-20`

5 seconds. Explained in 7.3.

### 7.2 `main()` — `cmd/core/main.go:22-42`

| Line | Code | Meaning |
|---|---|---|
| `:23-26` | flags `-addr`, `-worker`, `-threshold` | defaults from env vars `ADDR`, `WORKER_URL` |
| `:28` | `reg := window.NewRegistry()` | device memory |
| `:29-34` | `scorer := &infer.Scorer{...}` | worker client + Go fallback, 300 ms timeout |
| `:35` | `go detect(reg, scorer, *threshold)` | start detect loop **in the background** |
| `:37-39` | routes: `/ingest` → ingest handler, `/healthz` → "ok" | |
| `:41` | `http.ListenAndServe(...)` | serve forever |

**`go detect(...)`** — the `go` keyword runs a function in parallel (a goroutine). After this line, two things run at once: the HTTP server (`:41`) and the detect loop. They share `reg`.

### 7.3 `detect()` — the heartbeat — `cmd/core/main.go:45-64`

```go
lastFall := map[string]int64{}                          // :46  last FALL time per device
for range time.Tick(window.Stride) {                    // :47  every 0.5 s:
    ws := reg.CutAll()                                  // :48    1. new windows from all devices
    if len(ws) == 0 { continue }
    scores, version := scorer.Score(..., ws)            // :52    2. score them all in ONE request
    for i, w := range ws {
        if scores[i] < threshold ||                     // :54    3a. not a fall → skip
           w.End()-lastFall[w.DeviceID] < cooldown {    //        3b. just printed FALL for this device → skip
            continue
        }
        lastFall[w.DeviceID] = w.End()
        log.Printf("FALL device=... score=... model=... window_end=... latency=...")  // :58
    }
}
```

**Why the cooldown?** One impact appears in **4 overlapping windows** (see 3.4). Without cooldown you'd see FALL printed 4 times for one fall. After a FALL, that device stays quiet for 5 s of sample time.

**Example log line:**
```
FALL device=replay score=1.00 model=threshold-py-v0 window_end=05:51:57.313 latency=440ms
```
- `model=threshold-py-v0` → Python scored it. `threshold-go-v0` → worker was down, Go fallback scored it.
- `latency=440ms` → from batch arrival to FALL printed. Phase 1 target: under 2 s.

### 7.4 `envOr()` — `cmd/core/main.go:66-71`

Read an env var, or use a default.

---

## 8. Docker files

### 8.1 `Dockerfile` (Go) — two-stage build

| Lines | Stage | What |
|---|---|---|
| `Dockerfile:1-6` | **build** | big `golang` image; copy code; compile `core` and `replay` into `/out/` |
| `Dockerfile:8-11` | **run** | tiny `alpine` image; copy only the 2 binaries; default command `core` |

Why two stages? The compiler is ~300 MB and isn't needed at runtime. Final image is tiny.

### 8.2 `worker/Dockerfile`

Python slim image → copy `server.py` → run it.

### 8.3 `docker-compose.yml`

| Lines | Service | Notes |
|---|---|---|
| `:2-10` | `core` | host port `${CORE_PORT:-8080}` (8080 unless you set `CORE_PORT`); `WORKER_URL=http://worker:8000` |
| `:12-14` | `worker` | not exposed outside — only `core` talks to it |
| `:17-25` | `replay` | `profiles: ["tools"]` → does NOT start with `up`, only with `docker compose run`; mounts `./data` and `./testdata` read-only |

- Inside Compose, containers reach each other **by service name** — that's why `http://worker:8000` works.
- `restart: unless-stopped` → container crashes, Docker restarts it.
- `depends_on` → starting `replay` also starts `core` (and `core` starts `worker`). Use `--no-deps` to stop that.

### 8.4 `.dockerignore`

Keeps `data/`, `testdata/`, `worker/` out of the Go build context → faster builds.

---

## 9. Tests — what each one proves

Run: `go test ./...` and `cd worker && python -m unittest`

| Test | Location | Proves |
|---|---|---|
| `TestThreshold` | `internal/infer/threshold_test.go:29` | rule fires on a fall; not on standing, bump, late spike, sit-down |
| `TestCutStridesBySampleTime` | `internal/window/window_test.go:18` | exactly 6 windows with correct start/end; no duplicates on 2nd call |
| `TestAddDropsOldAndDuplicateSamples` | `internal/window/window_test.go:41` | re-sent data isn't counted twice |
| `TestRingOverwritesOldest` | `internal/window/window_test.go:50` | buffer never grows past 2048 |
| `TestCutSkipsWindowsWithGaps` | `internal/window/window_test.go:58` | paused phone doesn't produce broken windows |
| `TestMergePrefersTotalAccelerationAndHoldsGyro` | `internal/ingest/ingest_test.go:20` | unit conversion + sample-and-hold |
| `TestMergeAddsGravityBackToAccelerometer` | `internal/ingest/ingest_test.go:35` | gravity-free accelerometer gets fixed |
| `TestHandlerStatus` | `internal/ingest/ingest_test.go:47` | bad requests get 401 / 405 / 400 |
| `test_*` (4 tests) | `worker/test_server.py:14-24` | Python rule matches Go rule |

**Tip:** tests are the best documentation. Each test is a concrete "this input → this output" example.

---

## 10. Go concepts cheat sheet

Things you'll see all over this code:

| Concept | Example | Meaning |
|---|---|---|
| struct | `type Sample struct { T int64; Ax float64 }` | a group of named fields (like a Python class with only data) |
| method | `func (b *Buffer) Add(...)` | a function attached to a type; `b` is like `self` |
| pointer `*` | `*Buffer` | "the actual thing, not a copy" — needed when a method changes the struct |
| `&` | `&merger{}` | create it and give me a pointer to it |
| slice | `[]Sample` | growable list |
| array | `[2048]Sample` | fixed-size list (used for the ring buffer) |
| map | `map[string]*Buffer` | dictionary |
| `:=` | `x := 5` | declare + assign |
| multiple returns | `scores, version := s.Score(...)` | functions can return several values |
| error handling | `if err != nil { return err }` | Go has no exceptions — errors are returned values |
| goroutine | `go detect(...)` | run a function concurrently |
| mutex | `mu.Lock() ... mu.Unlock()` | only one goroutine at a time in this section |
| `defer` | `defer mu.Unlock()` | run this when the function exits |
| JSON tag | `` `json:"name"` `` | which JSON key maps to this field |
| `time.Tick` | `for range time.Tick(500ms)` | loop body runs every 500 ms |
| `int64` time | `1700000000020000000` | unix time in **nanoseconds** (1 s = 1,000,000,000) |

---

## 11. Exercises — how to actually learn this

Reading ≠ understanding. **Predicting** = understanding. For each exercise, **write down your guess first**, then run it.

1. **Break the rule.** In `internal/infer/threshold.go:21` change `ImpactG: 2.5` → `5.0`. Run `go test ./internal/infer`. Which test fails, and why?
2. **Remove the cooldown.** In `cmd/core/main.go:20` set `cooldown` to `0`. Rebuild and replay the synthetic fall. How many FALL lines? (Hint: section 3.4.)
3. **Kill the worker.**
   ```sh
   docker compose stop worker
   docker compose run --rm --no-deps replay /testdata/synthetic/F00_SYNTH_fall.txt
   docker compose logs core
   ```
   What does `model=` say now? What's the latency, and why is it higher?
4. **Trace by hand.** Open `testdata/synthetic/F00_SYNTH_fall.txt`. Find the line where the numbers jump. Compute the magnitude in g yourself (raw ÷ 256, then √(x²+y²+z²)). Does it cross 2.5?
5. **Add a log line.** In `internal/window/window.go` inside `Cut()`, print how many windows were cut. Replay. Does the count match what section 3.4 predicts?
6. **Explain out loud** (to a friend, a rubber duck, or an interviewer):
   - Why does the window use sample time instead of wall time?
   - Why does the scorer never return without scores?
   - Why does ingest add gravity back to the accelerometer?
   - Why is there a cooldown?

   If you can answer all four clearly, you understand this codebase.

### What to own vs. vibe-code

The design doc's rule 2: *write the core yourself.* For this codebase that means understanding these **deeply** — interviewers will ask:
- `internal/window/window.go` — ring buffer + windowing
- `internal/infer/threshold.go` — the fall rule
- `internal/infer/client.go` — fallback logic
- `cmd/core/main.go` → `detect()` — the loop + cooldown

Fine to understand at a high level: CSV parsing in replay, Docker files, HTTP boilerplate.
