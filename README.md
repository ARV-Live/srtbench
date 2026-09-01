# srtbench

[![ci](https://github.com/ARV-Live/srtbench/actions/workflows/ci.yml/badge.svg)](https://github.com/ARV-Live/srtbench/actions/workflows/ci.yml)
[![docker](https://github.com/ARV-Live/srtbench/actions/workflows/docker.yml/badge.svg)](https://github.com/ARV-Live/srtbench/actions/workflows/docker.yml)
[![License](https://img.shields.io/badge/license-Apache--2.0-blue.svg)](LICENSE)

Streams test video **and audio** over SRT, measures the received stream, and
publishes a MOS score plus supporting telemetry to InfluxDB 2.x.

Standalone: no shared infrastructure, no service to deploy. It points at an
InfluxDB you already have, or writes CSV and needs nothing at all.

## Install

**Docker** — the image carries an ffmpeg with both libsrt and libvmaf, which
most distribution packages do not:

```
docker run --rm ghcr.io/arv-live/srtbench run     -endpoint srt://127.0.0.1:8890 -csv - -duration 30 -v
```

**From source** — Go 1.25+, plus ffmpeg and ffprobe on PATH:

```
go install github.com/ARV-Live/srtbench/cmd/srtbench@latest
```

ffmpeg must be built with `--enable-libsrt` for streaming and
`--enable-libvmaf` for the full-reference pass. Check with
`ffmpeg -version | grep -o 'libsrt\|libvmaf'`; without libvmaf everything works
except the ground-truth measurement and calibration.

## Usage

```
go build -o srtbench ./cmd/srtbench

# one-command demo: sender and receiver wired together locally
./srtbench run -endpoint srt://127.0.0.1:9000 -csv - -duration 30 -v

# measure a real stream from OBS / Moblin / an encoder
./srtbench receive -endpoint srt://0.0.0.0:8890 -influx-url http://influx:8086 \
    -influx-org myorg -influx-bucket srtbench -influx-token $TOKEN

# push test media at something else
./srtbench send -endpoint 'srt://ingest.example:8890?streamid=publish:key' -bitrate 4500
```

## Caller or listener?

SRT connections have a direction, and picking the wrong one is the most common
way to get stuck. `receive` defaults to **listener** — it binds a local port and
waits for an encoder to push to it. That is right when the stream comes to you,
and wrong whenever the endpoint is somewhere else.

| You want to | Command |
|---|---|
| Accept a stream from OBS / Moblin on this box | `srtbench receive -endpoint 'srt://0.0.0.0:8890'` |
| Push test media **to** a remote ingest | `srtbench send -endpoint 'srt://ingest.example:8890?streamid=publish:<key>'` |
| Pull a stream **from** a remote endpoint and measure it | `srtbench receive -mode caller -endpoint 'srt://ingest.example:8890?streamid=read:<key>'` |

You cannot listen on an address belonging to another machine, so srtbench
detects that case and prints the three options above rather than letting the
bind fail with the operating system's "cannot assign requested address".

### Round-tripping your own ingest with `run`

Give `run` a publish id and a read id and it drives both directions against the
same server: the sender pushes in, the receiver pulls the same stream back, and
the score covers the whole path.

```yaml
srt:
  endpoint: "srt://ingest.example:8890"
  publish_streamid: "publish:<key>"
  read_streamid: "read:<key>"
```

```bash
./srtbench run -endpoint 'srt://ingest.example:8890'     -publish-streamid 'publish:<key>' -read-streamid 'read:<key>'     -bitrate 4500 -csv - -v
```

The publisher starts first on purpose — a server has no stream to hand out
until something is publishing, so the reader would be refused. The reader
retries for 20 s to cover the rest of the startup.

Both ids are used **verbatim**; the `publish:` / `read:` convention is
MediaMTX's, not srtbench's. Setting only one is refused rather than quietly
falling back to a loopback that never touches your server — that would report a
healthy score for a path nobody tested.

**A caveat that will otherwise mislead you.** A server that re-muxes — MediaMTX
and every other SRT server — terminates the SRT session and emits a *fresh*
stream, regenerating the MPEG-TS continuity counters. On a round trip those
counters describe the server's output, not your encoder's, so transport damage
upstream of the server does not appear as loss. Measured here against MediaMTX
with 2% burst loss injected into the publish leg:

| | MOS | video | loss % | fps delivered | k_freeze |
|---|---|---|---|---|---|
| clean | 4.01 | 3.35 | 0.0000 | 29.4 | 1.000 |
| 2% burst loss | 3.73 | 2.87 | **0.0000** | 27.2 | **0.814** |

The damage is real and the MOS correctly falls, but it arrives as **freezes and
dropped frames**, not as packet loss. So on a round trip, read `freeze_ms`,
`k_freeze` and `fr_delivered` as the damage indicators, and do not read a 0%
loss figure as "the path is clean". For loss attribution you need srtbench
terminating the SRT session itself — `receive` in listener mode, with the
encoder pointed straight at it.

Hostnames work on both paths, and both resolve **IPv4 first**. That is not
cosmetic: `localhost` answers `::1` before `127.0.0.1` on most systems, a
listener on `0.0.0.0` is IPv4-only, and the mismatch produces no error at
either end — the connection simply never happens.

## What the MOS number means, and what it does not

**Read this before quoting a score.**

The parametric MOS is structured after **ITU-T G.1070** — a video term, an
audio term, and their multimedia integration. The *structure* is from the
standard. **The coefficients are not.**

G.1070's published `v1..v12` tables were fitted for MPEG-4/H.264 at QQVGA and
QVGA. Transplanting them onto 1080p HEVC would be worse than a transparent
guess, because the bitrate-scale parameters are an order of magnitude off for
this operating point. So every shipped video coefficient is **derived from
stated anchor points** — the derivations are written out in
`internal/qoe/defaults.go` so you can redo or dispute them — and labelled
`estimated`. The freeze and stall constants are labelled `placeholder`.

`./srtbench profiles` prints that provenance, and every point written to
InfluxDB carries a `calibrated` field.

> **Uncalibrated, this is a reliable *relative* indicator.** It is excellent at
> detecting degradation, comparing runs, and driving alerts. It is **not** an
> absolute subjective opinion score until calibrated against VMAF for your
> codec, resolution and content.

Shipping a placeholder that is labelled as one beats shipping a
confident-looking number that quietly is one.

## How it works

```
SRT socket (owned by srtbench)  ──┬──> SRT statistics, 1 Hz
                                  ├──> MPEG-TS parser: per-PID CC, PCR, PES PTS
                                  ├──> ffmpeg decoder: frames, errors, freezes,
                                  │    silencedetect
                                  └──> QoE engine ──> InfluxDB / CSV
```

**The tool owns the SRT socket, and everything rests on that.** ffmpeg's
`srt://` protocol exposes no libsrt runtime statistics, so RTT, retransmissions
and — critically — *late-drop* counts are simply unavailable through it.
Without those a MOS score is guesswork. ffmpeg is used only for encoding and
decoding, which is what it is genuinely good at.

### Three loss sources, one answer

| Source | Role |
|---|---|
| **TS continuity counters, per PID** | **Authoritative.** The only source that says *which* stream was damaged. |
| **SRT `PktRecvDrop`** | Magnitude corrector, and the sole fallback. |
| **Decoder errors** | Corroboration only. Never enters the arithmetic. |

`PktRecvLoss` is deliberately **excluded**: it counts packets *initially*
missing, almost all of which ARQ recovers, so using it would report
catastrophic loss on a link that delivered a perfect picture. `PktRecvDrop` —
SRT giving up on a packet past its deadline — is the residual loss that
actually damages the image.

The continuity counter is 4 bits, so losing exactly 16 consecutive packets on a
PID produces *zero* discontinuities. SRT's drop count has no such wrap and is
used to correct the magnitude.

### Why per-PID accounting is not optional

A real 1080p H.264/AAC stream measured here carries **2216 video TS packets per
second against 179 audio** — a 12:1 ratio. One lost SRT packet is 7 TS packets:
landing on video that is 0.32% loss, landing on audio it is 3.8%. A single
stream-wide loss figure would be wrong by an order of magnitude in one
direction or the other, and would make the audio score meaningless.

The same ratio forces a second decision: at 179 audio packets/s, the smallest
non-zero audio loss in a 1 s window is 0.56%, which alone moves audio MOS by
**0.42** — one packet. Audio loss is therefore accumulated over 5 s, bringing
that quantum to 0.065.

### Things that are deliberately *not* penalised

- **A constant A/V offset.** MPEG-TS muxers run video ahead of audio by design;
  a healthy stream measured here sits at −242 ms. Penalising the raw offset
  would fire on every stream ever measured. The tool learns each stream's own
  baseline and scores only the *deviation* from it.
- **Silence.** An IRL streamer who stops talking is not a quality defect.
  Silence only counts once the frame count corroborates that audio is dead.
- **Latency.** A correctly configured 3000 ms SRT link is not broken. Latency
  is reported as its own metric, never folded into MOS.
- **Our own bottleneck.** If the decoder feed backs up, the window is discarded
  and loudly logged. A tool that scores an impairment it created itself is
  worthless.

## Benchmarking

Because the tool owns the socket, it can *cause* degradation, not just observe
it:

```
./srtbench run -impair-loss 2 -impair-burst 3 -csv sweep.csv -duration 60
```

`-impair-burst` matters more than the rate for realism: mobile uplinks fail in
bursts, and burst loss is far more damaging than the same percentage spread
uniformly. The seed is fixed, so a degradation curve is reproducible.

Measured on a clean loopback, 1280x720@30 at 2500 kbps:

| Condition | MOS | State |
|---|---|---|
| clean | 4.48 – 4.53 | excellent |
| 2% burst loss | 1.6 – 3.0 | poor |

Note the ARQ story visible in that second row: **3% injected loss arrives as
0.6% residual**. Almost all of it is recovered by retransmission, and only what
SRT abandons past its deadline actually damages the picture. That gap is the
whole reason the model consumes `PktRecvDrop` rather than `PktRecvLoss`.

## The calibration gap, measured

The full-reference pass makes the parametric model's error visible, and it is
not small. Same runs, `h264-720p`:

| Condition | VMAF | VMAF -> MOS | Parametric MOS | Gap |
|---|---|---|---|---|
| clean | 98.4 | 4.75 | 4.00 | −0.75 |
| 2% burst loss | 79.7 | 4.30 | ~2.50 | −1.80 |

**The shipped coefficients are consistently pessimistic against VMAF, and much
more so under loss.** That points squarely at the `V8..V12` loss-sensitivity
block being too harsh — which was already the third-ranked uncertainty in
`internal/qoe/defaults.go` before any of this was measured.

This is exactly what the calibration path exists to fix, and it is the clearest
possible argument for not quoting an uncalibrated score as an absolute. The
*ordering* is sound — clean beats lossy, every time, monotonically. The
*absolute values* are not yet trustworthy.

## Configuration

Flags > `SRTBENCH_*` environment > YAML > defaults. See `configs/example.yaml`.

Useful environment variables: `SRTBENCH_ENDPOINT`, `SRTBENCH_INFLUX_URL`,
`SRTBENCH_INFLUX_TOKEN`, `SRTBENCH_INFLUX_ORG`, `SRTBENCH_INFLUX_BUCKET`,
`SRTBENCH_PROFILE`, `SRTBENCH_CSV`.

H.264 is the default codec because libx265 is far heavier to encode, and a
sender that cannot keep up produces degradation indistinguishable from network
damage — you would be measuring the test rig.

## Measurements written

Tags on every point: `session_id`, `host`, `role`, `profile`, `stream_id`,
`state`.

| Measurement | Contents |
|---|---|
| `qoe` | `mos_overall`, `mos_video`, `mos_audio`, plus every model intermediate and `calibrated` |
| `srt_transport` | packets, loss, retransmits, drops, RTT, buffer, link capacity |
| `ts_stream` | per-PID continuity errors and packet counts, PCR jitter, PIDs |
| `media_video` | frames, dups, drops, decode errors, freezes |
| `media_audio` | decode errors, silence, gaps — *absent when there is no audio track* |
| `av_sync` | drift from the stream's own baseline |

`mos_audio` is **absent**, never zero, on a video-only stream; `mos_overall`
falls back to `mos_video`. Writing zero would drag every dashboard mean down by
about 1.5 MOS with a cause that is very hard to find later. All counters are
per-window deltas, never lifetime totals.

An unscoreable window (warmup, no connection, counter reset) is written with
`valid=false` and a `reason`, and **no MOS value at all**.

## Grafana dashboard

`grafana/srtbench-dashboard.json` visualises everything above. Import it via
**Dashboards -> New -> Import**, upload the file, and pick your InfluxDB
datasource when prompted (it uses the standard `__inputs` share format, so it
carries no hard-coded datasource UID). Two variables let you point it around:
**Bucket** (default `srtbench`) and **Session**.

22 panels in five rows:

| Row | What it answers |
|---|---|
| **Overview** | Six stat tiles: overall / video / audio MOS, residual loss, RTT, and whether the profile is calibrated |
| **Quality** | MOS over time with the VMAF ground truth overlaid, plus raw VMAF |
| **Loss & transport** | Per-stream effective loss, SRT packet outcomes, RTT, bitrate, buffer vs latency budget |
| **Stream & decode health** | Per-PID continuity errors, freezes, decode errors, A/V drift, PCR jitter, audio dropouts |
| **Diagnostics** | Impairment multipliers, confidence, and a table of unscored windows with reasons |

A few deliberate choices:

- **The MOS panel plots the estimate as lines and the VMAF ground truth as
  discrete violet markers.** The reference pass is sparse by design, and drawing
  it as a line would imply a continuity it does not have. The vertical distance
  between the two *is* the calibration error, visible at a glance.
- **`calibrated` is a headline tile**, not a footnote, and reads `UNCALIBRATED`
  in amber on the shipped profiles.
- **No panel has two y-axes.** Measures of different scale get their own panel.
- **Gaps are preserved** (`spanNulls: false`), so an unscored window reads as
  absent rather than being bridged into a plausible-looking line.
- **Colour is fixed per entity** — video is always orange, audio always aqua —
  so a filter never repaints a series. The status palette (green/amber/red) is
  reserved for MOS thresholds and never reused for a series, and the thresholds
  match srtbench's own state machine exactly.

The palette was validated for colour-vision deficiency and for contrast against
both the light and dark Grafana surfaces (Grafana applies one fixed colour per
series regardless of theme, so a single set has to clear both).

`grafana/generate_dashboard.py` regenerates the JSON if you want to change it;
editing the generator is easier than editing 73 KB of hand-written JSON.

### Flux starting point

```flux
from(bucket: "srtbench")
  |> range(start: -1h)
  |> filter(fn: (r) => r._measurement == "qoe" and r._field == "mos_overall")
  |> aggregateWindow(every: 10s, fn: mean)
```

## Testing

```
go test ./...
```

The TS parser and the QoE engine are pure — bytes and numbers in, numbers out,
no I/O — so the whole model is testable without a live stream. To additionally
run the parser against a real capture:

```
SRTBENCH_TEST_TS=/path/to/capture.ts go test ./internal/ts/ -run RealCapture -v
```

## Full-reference pass

Enabled by default (`qoe.reference`), it records a short segment on a duty
cycle and scores it against a regenerated reference, writing `vmaf`, `mos_vmaf`
and `vmaf_aligned` to the `srtbench_ref` measurement.

Three things about alignment, each of which cost a real debugging cycle:

1. **Align to the first KEYFRAME, not the first packet.** A segment cut from
   mid-stream starts mid-GOP, and the decoder skips everything until the first
   IDR — so the first frame that actually emerges is the keyframe. Measured in
   a fixture here, the gap between first packet and first keyframe is **1.07 s**,
   and aligning to the wrong one scores **VMAF 10 instead of 98**.
2. **Read packets, not frames.** `ffprobe -show_entries frame=...` returns an
   empty list on a mid-stream segment even though ffmpeg decodes it fine.
   Packet headers carry both the timestamp and the keyframe flag with no
   decoding required.
3. **ffmpeg prints the VMAF score at `info` level.** Running the analysis child
   at `-loglevel error` silently discards the result.

Alignment is verified rather than assumed: the test suite checks that a
correctly aligned mid-stream segment scores above 90 *and* that a one-second
misalignment scores dramatically worse. Without the second check, the first
could pass by coincidence.

The sender uses a 2 s GOP (`media.gop`). A capture shorter than one GOP
contains no keyframe, cannot be aligned, and is reported as an error rather
than scored against an arbitrary point.

## Calibration

Two commands turn the shipped estimates into something fitted to your own path.

```bash
# 1. Sweep an impairment grid, collecting parametric scores and VMAF together.
./srtbench sweep -csv sweep.csv -profile h264-720p     -sweep-bitrates 600,1200,2400 -sweep-loss 0,1,4 -sweep-seconds 45

# 2. Fit, and check the result before adopting it.
./srtbench calibrate -in sweep.csv -out h264-720p-fitted.yaml -profile h264-720p

# 3. Use it.
./srtbench receive -profile-file h264-720p-fitted.yaml -csv -
```

A real run on a 9-cell sweep:

```
  pairs            25 across 9 sweep cells
  held-out RMSE    1.080 -> 0.698 MOS
  Pearson  r       0.844 -> 0.797
  Spearman rho     0.871 -> 0.860   (ordering was already sound; the fit corrects the scale)
  correction       MOS' = 1.0306 + 0.9441 * MOS
```

That is the −0.75 gap from the section above, closed: the same clean stream
scored 4.01 against the shipped profile and 4.82 against the fitted one, which
is where VMAF put it.

### What it fits, and what it refuses to

**By default it fits two parameters**, an affine correction on the finished
score. Refitting all twelve video coefficients overfits badly — `V3`/`V4`/`V5`
trade off against `V10`–`V12` almost freely, so many very different parameter
sets explain the training data equally well and generalise *worse* than the
shipped guesses. Two parameters cannot overfit, they fix the failure that
actually dominates (a systematic offset and slope), and being monotone they
cannot reorder anything. `-refit-video` additionally refits the four
loss-sensitivity coefficients, under bounds and a ridge.

**Everything below is a refusal, not a limitation:**

- **Held out by sweep cell, never by window.** Windows inside one cell share an
  encoder state and a loss seed, so holding out random windows leaves
  near-duplicates of every test point in the training set. That reports a
  spectacular error which is pure leakage, and it is the easiest way to convince
  yourself a bad calibration is a good one. Every figure printed is held out.
- **A fit that does not beat doing nothing is discarded**, and the tool says so
  rather than writing a file. A calibration that fails to generalise is worse
  than none, because it carries the authority of having been measured.
- **Fewer than three cells is refused outright** — there is nothing to hold out.
- **A ridge pulls toward the shipped defaults**, so a thin sweep degrades into
  "slightly adjusted defaults" rather than a confidently wrong new model.
- **An inverting correction is never shipped**, whatever the residual says: a
  negative slope would rank a good stream below a bad one.
- **Unaligned VMAF segments are excluded.** A misaligned comparison still
  returns a plausible number — it simply compared the wrong frames.
- **The audio and multimedia blocks stay `estimated`.** VMAF carries no audio
  information, and no full-reference metric says how a viewer combines audio
  with video; that needs subjective data. Only the correction is marked
  `fitted`, and `srtbench profiles` and the `calibrated` field both reflect it.

`-refit-video` is checked against the affine fit, not merely against doing
nothing, and it says so when it loses. On the sweep above it did:

```
  WARNING: the video refit is WORSE out of sample than the two-parameter
  fit alone (0.931 vs 0.698 held-out RMSE). That is overfitting.
  Re-run without -refit-video.

  WARNING: V4 reached the edge of the allowed range. This data does not
  determine it; widen or lengthen the sweep, or drop -refit-video.
```

A coefficient pinned to the edge of its range means the search kept pushing and
the data never pushed back — the fit is straining against something the sweep
does not determine.

Watch **Spearman** more closely than Pearson. A model that ranks streams
correctly but is offset is exactly what the affine correction repairs; one that
ranks them *wrongly* is broken in a way no correction can fix, and only the rank
correlation reveals that.

A fitted profile layers onto its base, carrying only what changed, so the blocks
calibration could not inform keep their documented defaults instead of loading
as zeros.

## Contributing

Issues and pull requests are welcome. Two things to know before opening one:

- `go test ./...` must pass, and `gofmt -l .` must print nothing. CI checks
  both. The VMAF tests skip automatically when the local ffmpeg lacks libvmaf.
- The comments explain *why*, not what. If a decision looks arbitrary it
  probably was not, and the reason is worth writing down — most of the
  non-obvious code here exists because a plausible-looking alternative was
  measured and found wrong.

## Licence

Apache-2.0. See [LICENSE](LICENSE) and [NOTICE](NOTICE).

The quality model is *structured* after ITU-T G.1070, but the coefficients
shipped here are not the standard's published values and must not be cited as
such. `srtbench profiles` prints what each one actually claims about itself.
