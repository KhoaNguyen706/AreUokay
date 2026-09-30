# Fall Detection Platform: Design Doc and Game Plan

Sep 29, 2026 · @Khoa Nguyen

## Summary

A live beta that spots falls from phone motion data in real time and alerts a caregiver when the person doesn't get up. Go carries the backend and infra side; Python carries the deep learning side, so one project feeds both of your resume versions.

- **Who uses it:** a wearer running Sensor Logger on their phone, a caregiver who gets the alert, and you as the operator.
- **Why it stands out:** a running system with real load numbers, a stateful alert engine, honest ML evaluation and real users. Most fall-detection repos are a single notebook.
- **Timeline:** about 8 weeks alongside classes, ending in an open beta with a sign-up cap.
- **What it is not:** a safety device. It is a labeled beta, and nobody should rely on it in an emergency.

## Goals and non-goals

v1 is done when beta users stream from their own phones and get correct alerts, with measured numbers for both resume versions.

**Goals for v1**

- Detect falls from live phone data and alert the caregiver about 40 seconds after a fall, including a 30-second "Are you OK?" wait.
- Handle 1,000 simulated wearers on one small server, with measured p99 latency.
- Beat a simple threshold baseline on false alarms, tested on people the model never trained on.
- Never go silent: fall back to the threshold rule when the model is down or overloaded.
- Run live as an open beta with sign-up, tokens, consent and data deletion.

**Not in v1 (the v2 list)**

- A second model for activity recognition
- Camera or computer vision input
- Your own phone app, or the M5Stick wearable with its buzzer
- Kafka, Kubernetes, autoscaling
- SMS or phone-call alerts
- Any web UI beyond five simple pages
- Cat's cradle, which is project 2 after v1 ships

## Users and flows

Three people touch the system, and four flows cover everything it does.

| Actor | What they do | What they see |
| --- | --- | --- |
| Wearer | Carries a phone running Sensor Logger | An "Are you OK?" push only when a fall is suspected |
| Caregiver | Gets alerted if the wearer doesn't answer | A push with the wearer's name and the time |
| You (operator) | Run the system and update the model | Grafana dashboards, logs and an uptime alert |

**Onboarding (once per wearer)**

1. Sign up on the web app from the phone.
2. Copy the personal Push URL, which contains the device token.
3. In Sensor Logger: Logger page → gear icon → Data Streaming → Enable HTTP Push → paste the Push URL ([setup steps](https://github.com/timeplus-io/proton/pull/489/files)).
4. Add a caregiver, install ntfy, and subscribe to the personal alert topic the web app shows.
5. Tap Send test alert to check the whole path, then start a recording.

**Live fall**

1. The phone pushes a batch of readings every second.
2. Go adds them to that wearer's recent history and cuts a 2-second window every half second.
3. The Python worker scores each window; a score over the threshold marks a possible fall.
4. The wearer gets "Are you OK?" with an I'm OK button.
5. No tap and no movement for 30 seconds: the caregiver is alerted.
6. No caregiver acknowledgment in 5 minutes: the backup contact is alerted.

**False alarm (sat down hard, dropped the phone)**

1. The wearer taps I'm OK or moves normally within 30 seconds.
2. The alert closes, and that window is saved as a "not a fall" example for retraining.

**Silent device**

1. An active device sends nothing for 60 seconds.
2. The wearer is told their phone stopped streaming, for example because of Low Power Mode or no signal.

## High-level design

One Go service does the fast, stateful work; one Python service does the math. Everything runs in Docker Compose on one server.

&#91;embedded content: architecture · 7 parts, phone to alert\]

Phones and the load simulator hit the same ingest path. The Go core keeps each device's state in memory, asks the Python worker for scores over gRPC, writes to PostgreSQL and sends alerts through ntfy.

| Component | Built with | Job |
| --- | --- | --- |
| Sensor Logger | iPhone app | Pushes a JSON batch of motion readings every second |
| Caddy | Reverse proxy | HTTPS certificates for everything public |
| Go core | Go | Ingest, per-device windows, batching, fallback, alert engine, web pages, auth |
| Inference worker | Python, ONNX Runtime | Preprocesses windows and returns fall scores over gRPC |
| PostgreSQL | Database | Users, devices, caregivers, alerts, raw batches, labels |
| ntfy | Self-hosted push server | "Are you OK?" prompts and caregiver alerts with action buttons |
| Prometheus + Grafana | Monitoring | Metrics and dashboards for you |
| Load simulator | Go | Replays SisFall and your recordings as up to 1,000 fake wearers |
| Training pipeline | Python, offline | Builds, tests and exports the model |

The Go core is one service split into packages: ingest, window, infer, alert, notify, web and store. One service is easier to run and debug, and a package can become its own service later if load demands it.

## How data moves

The live path never waits on the database. Data flows through memory to the model and the alert engine, and storage happens on the side.

**Ingest.** `POST /ingest?token=…` checks the token, applies a per-token rate limit and answers fast. Readings go into that device's ring buffer, which holds the last 10 seconds.

**Windowing.** Every 0.5 seconds, Go cuts the latest 2 seconds of each active device into a window of raw readings with timestamps.

**Batching.** Windows from all devices wait in one bounded queue. Go sends a batch when it has 64 windows or 10 ms have passed, whichever comes first.

**Overload.** When the queue is full, overflow windows are scored by the threshold rule in Go instead of waiting. Accuracy dips under overload, but nothing is skipped.

**Inference.** The Python worker resamples each window to 50 Hz, normalizes it, runs the 1D-CNN and returns one fall score per window. Several workers can run side by side.

**Fallback.** If the worker is down or times out, Go scores windows with the threshold rule itself. The system gets less accurate, never silent.

**Alert engine.** Each device has a small state machine. Every state change is written to PostgreSQL, so a restart can pick up open alerts and their timers.

&#91;embedded content: alert state machine · 6 states\]

A possible fall closes when the wearer taps I'm OK or moves. Otherwise it climbs to the caregiver, then the backup contact, until a person acknowledges it.

**Notifications.** Each wearer and caregiver gets a random, secret ntfy topic. The I'm OK button is an ntfy HTTP action that calls `POST /alerts/{id}/ok` with a signed link ([ntfy docs](https://docs.ntfy.sh/publish/)).

**How the phone rings.** A server can't run a script, play audio or set an alarm on a phone. It can only send a push notification to an app the person installed.

- ntfy: a notification sound; on iPhone it follows silent mode and Do Not Disturb.
- [Pushover](https://pushover.net/api) emergency alerts: repeat every 30 seconds until acknowledged, and ring on silent with Critical Alerts turned on.
- A true alarm that keeps ringing: your own app or the M5Stick buzzer, in v2.

**Storage.** Each 1-second batch is saved as one row, not one row per reading. One row per reading would mean 100 × 86,400 = 8.64 million rows per phone per day. Raw batches are deleted after 7 days; alerts, state changes and labels are kept.

## API and data model

Phones, notification buttons and browsers talk to Go over HTTPS; Go talks to Python through one gRPC call; seven tables hold the state.

| Endpoint | Caller | Purpose |
| --- | --- | --- |
| `POST /ingest?token=…` | Phone | Receive a batch of readings |
| `POST /alerts/{id}/ok` | Wearer's ntfy button | Cancel a possible fall |
| `POST /alerts/{id}/ack` | Caregiver's ntfy button | Stop escalation |
| `/signup`, `/login`, `/device`, `/caregivers`, `/test-alert`, `/delete-account` | Browser | Onboarding and account pages |
| `GET /healthz`, `GET /metrics` | Uptime check, Prometheus | Health and metrics |

The gRPC contract is `Inference.Score(WindowBatch) → ScoreBatch`. Each window carries a device ID, timestamps and six channels: accelerometer and gyroscope x, y, z. The reply carries one score per window plus the model version.

| Table | Key columns | Notes |
| --- | --- | --- |
| users | id, email, password\_hash, created\_at | Passwords hashed with bcrypt |
| devices | id, user\_id, token\_hash, last\_seen\_at | Tokens stored hashed, like passwords |
| caregivers | id, user\_id, name, ntfy\_topic, position | Position sets who is alerted first |
| alerts | id, device\_id, state, score, model\_version, detected\_at, resolved\_at | One row per possible fall |
| alert\_events | id, alert\_id, from\_state, to\_state, at | Audit trail of every state change |
| sensor\_batches | device\_id, received\_at, payload | One row per 1-second batch, deleted after 7 days |
| labels | id, device\_id, window\_start, window\_end, label, source | I'm OK windows become "not a fall" examples |

## ML design

The model is a small 1D-CNN that must beat a threshold baseline on false alarms, tested only on people it never trained on.

**Data**

- [SisFall](https://www.researchgate.net/publication/312669325_SisFall_A_Fall_and_Movement_Dataset): falls and daily activities recorded at 200 Hz with a sensor on the waist. Falls were acted out mostly by young volunteers; older volunteers mainly did daily activities.
- Your own Sensor Logger recordings: walking, stairs, sitting down hard, and phone drops onto a bed. These are "not a fall" examples SisFall doesn't have.
- Later: I'm OK windows from beta users.
- Stretch goal: KFall, another fall dataset, for a cross-dataset test.

**Preprocessing (one Python module, used by training and by the worker)**

- Convert units to g and degrees per second; SisFall's documentation gives the conversion from its raw values.
- Resample everything to 50 Hz.
- Windows of 2 seconds (100 samples × 6 channels), a new one every 0.5 seconds.
- Label a window "fall" when it contains the impact of a fall recording; everything else is "not a fall".
- Training and serving call the same code, so what the model sees live matches what it trained on. This prevents training-serving skew.

**Split by person**

- Train, validation and test sets hold different people. A random split by window puts the same person in both training and testing and inflates accuracy.

**Models**

1. Baseline: a free-fall dip followed by an impact spike in magnitude = √(x² + y² + z²). The same rule is the production fallback.
2. 1D-CNN in PyTorch: a few 1D convolution blocks, global pooling and one output. Use a weighted loss, because falls are rare.

**Metrics (test-set people only)**

- Fall recall: the share of real falls caught.
- False alarms per hour of normal activity.
- Precision, PR-AUC and inference time per window.
- The bar to clear: fewer false alarms than the baseline at the same recall.

**Export**

- An ONNX file with the version in its name. The worker reports the version, and every alert stores it.

## Non-functional targets

These are starting targets. The load test and the beta decide whether they hold, and the measured numbers go on your resume.

| Area | Target | How you check it |
| --- | --- | --- |
| Detection latency | Possible fall flagged within 2 s of the data arriving (p99) | Timestamps on each window, a Prometheus histogram |
| Alert time | Caregiver alerted about 40 s after a fall, including the 30 s wait | A phone falling onto a mattress, end to end |
| Throughput | 1,000 wearers × 100 readings/s = 100,000 readings/s, and 2,000 windows/s | Load simulator against the deployed server |
| Overload | At 2x load, overflow goes to the threshold rule and nothing is skipped | Load test with 2,000 wearers |
| Reliability | Containers restart on crash; open alerts survive a restart | Kill containers during a load test |
| Security | HTTPS only, hashed passwords and tokens, per-token rate limits | A checklist before the beta |
| Privacy | Raw data deleted after 7 days; delete-account removes everything | Retention job plus a test account |
| Cost | $0 on new-account AWS credits for up to 6 months, then about $15–18 a month | AWS billing page and a budget alert |

Cost basis: the [AWS Free Tier](https://aws.amazon.com/about-aws/whats-new/2025/07/aws-free-tier-credits-month-free-plan/) credits and [t4g.small pricing](https://aws-pricing.com/t4g.small.html); the extra few dollars cover the public IP and the disk.

## Tech stack

Go and Python are the core; everything else is standard, free and runs in Docker.

| Layer | Choice | Why |
| --- | --- | --- |
| Phone | [Sensor Logger](https://apps.apple.com/us/app/sensor-logger/id1531582925) on iPhone | Free, streams motion data over HTTP |
| Backend | Go: net/http (or Gin), pgx, grpc-go, bcrypt, html/template | Simple concurrency, fast, pages without React |
| ML training | Python: PyTorch, NumPy, pandas, scikit-learn | Standard deep learning stack |
| ML serving | Python: ONNX Runtime, grpcio | Fast inference from one exported file |
| Contract | Protocol Buffers + gRPC | Typed calls between Go and Python |
| Database | PostgreSQL | Users, alerts, labels, raw batches |
| Notifications | [ntfy](https://docs.ntfy.sh/publish/), self-hosted | Free push with priorities and action buttons |
| Monitoring | Prometheus, Grafana, an uptime check | Metrics, dashboards and an alert to you when it breaks |
| Proxy | Caddy | Automatic HTTPS certificates |
| Packaging | Docker, Docker Compose | Same setup on your laptop and the server |
| CI/CD | GitHub Actions | Tests on every push, deploy on main |
| Hosting | AWS EC2 t4g.small (ARM), or [OVHcloud VPS-1](https://bestusavps.com/reviews/hetzner/) | [About $12 a month](https://aws-pricing.com/t4g.small.html) on AWS; OVHcloud about $5 |

t4g servers are ARM, so build ARM Docker images. Go and ONNX Runtime both support ARM.

## Phased build plan

Five phases over about 8 weeks, each ending in a "done when" check. You move to the next phase only when the check passes.

&#91;embedded content: roadmap · 5 phases, 5 gates\]

Each diamond is a phase's done-when check; the task lists below say what gets you there.

**Phase 0: Feel the data (Sep 29 – Oct 4)**

- [ ] Sensor Logger pushes to a Go server on your laptop that prints each batch
- [ ] Record walking, sitting down hard and a phone drop onto a bed, then export the CSV
- [ ] Download SisFall and plot one fall and one daily activity next to your recording

Done when: you can point at the free-fall dip and the impact spike on your own plot.

**Phase 1: Thin slice (Oct 5 – Oct 18)**

- [ ] Go ingest endpoint and per-device ring buffer
- [ ] Replay tool: Go streams a CSV (SisFall or yours) at real speed
- [ ] Windowing every 0.5 s and the threshold rule, printing FALL
- [ ] Python worker skeleton over gRPC (plain HTTP is fine at first)
- [ ] Docker Compose runs Go and Python together

Done when: a replayed SisFall fall prints FALL within 2 seconds, and so does your live phone.

**Phase 2: The model (Oct 19 – Nov 1)**

- [ ] Shared preprocessing module: units, 50 Hz resampling, windows, labels
- [ ] Person-level split, with baseline numbers written down
- [ ] Train the 1D-CNN and measure recall and false alarms per hour
- [ ] Export to ONNX, serve it from the worker, and fall back to the threshold rule when the worker is down

Done when: the CNN has fewer false alarms than the baseline at the same recall, on test-set people.

**Phase 3: Alerts, scale, deploy (Nov 2 – Nov 15)**

- [ ] Alert state machine, PostgreSQL and ntfy with the I'm OK button
- [ ] Silent-device alert
- [ ] Load simulator with 1,000 wearers; batching and overflow to the fallback
- [ ] Prometheus and Grafana dashboards
- [ ] Deploy to the server behind Caddy; GitHub Actions deploys on main

Done when: 1,000 simulated wearers run with a measured p99, and a phone falling onto a mattress alerts your caregiver phone through the deployed server.

**Phase 4: Live beta (Nov 16 – Nov 29)**

- [ ] Five web pages: sign up, device, caregivers, test alert, delete account
- [ ] Hashed tokens, rate limits, the 7-day retention job, backups and an uptime check
- [ ] Disclaimer, consent checkbox and privacy page
- [ ] Private beta: 3–5 friends for one week, counting real false alarms per day
- [ ] Retrain with the I'm OK windows
- [ ] Open beta with a cap of 50 users

Done when: a stranger can sign up and stream without your help, and you have a real-world false-alarm number.

**After v1: Ship the story**

- [ ] README with the architecture, the numbers and a short demo video
- [ ] Both resume versions updated with measured numbers
- [ ] Pick the next item from the v2 list

## Risks

The biggest risk is not technical: it is switching projects before this one ships.

| Risk | What you do about it |
| --- | --- |
| Switching to a new idea mid-build | New ideas go on the v2 list; no new project before Phase 3 is done |
| The model doesn't transfer from SisFall to phones | Same preprocessing for both, your own recordings in training, fine-tuning |
| Phone drops cause false alarms | Gyroscope channels, your own drop recordings as negatives, the I'm OK step |
| iPhone pauses background recording | Silent-device alert; test Low Power Mode and Focus modes ([Sensor Logger help](https://www.tszheichoi.com/sensorloggerhelp)) |
| Battery drain | Measure over a full day; lower the sampling rate if needed |
| ntfy action buttons may not work on iPhone | Test in Phase 3; switch the I'm OK button to [Pushover](https://pushover.net/api) if they don't ([iOS note](https://github.com/hbrennhaeuser/homeassistant_integration_ntfy)) |
| Notifications stay quiet in Do Not Disturb | State it as a known limit in the README; Pushover Critical Alerts later |
| Overload at scale | Bounded queue; overflow goes to the threshold rule |
| Privacy and safety | Beta label, consent, 7-day retention, delete-account |
| A phase runs long | Cut scope inside the phase; never switch projects |

## Resume bullets and interview talking points

Fill the brackets only with numbers you actually measured.

**Backend/infra version**

- Built a real-time fall detection platform in Go that ingests \[X\] sensor readings/sec from \[N\] simulated wearers, with p99 detection latency of \[Y\] ms.
- Designed a per-device alert state machine with caregiver escalation, persisted in PostgreSQL, that falls back to a threshold detector when the model service fails.
- Connected Go and Python over gRPC with dynamic batching and a bounded queue, raising throughput \[Z\]x and staying live at 2x overload.
- Deployed with Docker Compose, Caddy and GitHub Actions on AWS; live beta with \[U\] users.

**AI/ML version**

- Trained a 1D-CNN in PyTorch on SisFall with person-level splits, reaching \[R\]% fall recall at \[F\] false alarms/hour vs. \[F0\] for a threshold baseline.
- Built one preprocessing module shared by training and serving to prevent training-serving skew.
- Served the model with ONNX Runtime behind a Go service at \[Q\] predictions/sec, p99 \[L\] ms.
- Measured \[D\] real-world false alarms per day across \[U\] beta users and retrained on their I'm OK feedback.

**Talking points**

- Why a threshold fallback: silence is the worst failure for a safety alert.
- Why split by person: a random split leaks people into the test set.
- How you tell a phone drop from a person falling.
- Why one row per batch instead of one row per reading.
- Why preprocessing lives in one shared module.
- What changes at 100x scale: a queue like Kafka between ingest and inference, more workers, and state sharded across Go instances.

## Rules for staying on track

Four rules protect the plan from idea switching.

1. New ideas go on the v2 list, not into the project, until Phase 3 is done.
2. Write the core yourself: batching, the alert engine and the training loop. Use AI for throwaway parts like the web pages.
3. Move on only when a phase's "done when" check passes. If a phase runs long, cut scope inside it.
4. Every Sunday, spend ten minutes on a weekly check: what shipped, what's next, what's blocked.
