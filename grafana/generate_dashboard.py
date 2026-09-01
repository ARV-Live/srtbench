# -*- coding: utf-8 -*-
"""Generate the srtbench Grafana dashboard JSON."""
import json

DS = {"type": "influxdb", "uid": "${DS_INFLUXDB}"}

# Validated series palette. These are the dark-mode steps from the reference
# palette, chosen because they pass every check against BOTH the light and the
# dark surface -- Grafana applies one fixed colour per series regardless of the
# viewer's theme, so a single set has to work in both.
BLUE, ORANGE, AQUA, VIOLET, MAGENTA = "#3987e5", "#d95926", "#199e70", "#9085e9", "#d55181"
# Status palette. Reserved for thresholds; never reused as a series colour, so a
# status hue can never impersonate a measurement.
GOOD, WARN, SERIOUS, CRIT = "#0ca30c", "#fab219", "#ec835a", "#d03b3b"

panels = []
_next = [0]


def nid():
    _next[0] += 1
    return _next[0]


SESSION = "r.session_id =~ /^${session:regex}$/"

# A boolean field cannot be aggregated: aggregateWindow returns nothing for it and
# the panel reads "No data". Map it to an integer first, which also makes the 0/1
# value mappings on the stat panel fire.
BOOL_TO_INT = "  |> map(fn: (r) => ({r with _value: if r._value then 1 else 0}))\n"


def flux(meas, fields, fn="mean", extra=""):
    f = " or ".join('r._field == "%s"' % x for x in fields)
    return (
        'from(bucket: "${bucket}")\n'
        "  |> range(start: v.timeRangeStart, stop: v.timeRangeStop)\n"
        '  |> filter(fn: (r) => r._measurement == "%s")\n'
        "  |> filter(fn: (r) => %s)\n"
        "  |> filter(fn: (r) => %s)\n"
        "%s"
        "  |> aggregateWindow(every: v.windowPeriod, fn: %s, createEmpty: false)\n"
        '  |> keep(columns: ["_time", "_value", "_field"])' % (meas, f, SESSION, extra, fn)
    )


def target(q, ref="A"):
    return {"refId": ref, "datasource": DS, "query": q}


def row(title, y):
    panels.append({
        "type": "row", "title": title, "id": nid(),
        "gridPos": {"h": 1, "w": 24, "x": 0, "y": y},
        "collapsed": False, "panels": [],
    })


def color_overrides(mapping):
    out = []
    for name, col, extra in mapping:
        out.append({
            "matcher": {"id": "byName", "options": name},
            "properties": [{"id": "color", "value": {"mode": "fixed", "fixedColor": col}}] + extra,
        })
    return out


def ts(title, y, x, w, h, targets, unit="", overrides=None, desc="",
       minv=None, maxv=None, decimals=None, axis=""):
    d = {
        "color": {"mode": "palette-classic"},
        "custom": {
            "drawStyle": "line", "lineInterpolation": "linear",
            # Thin marks: 2px lines, recessive grid, no fill.
            "lineWidth": 2, "fillOpacity": 0, "gradientMode": "none",
            "pointSize": 5, "showPoints": "never",
            # A gap is data: an unscored window must read as absent, not bridged.
            "spanNulls": False,
            "axisPlacement": "auto", "axisLabel": axis,
            "scaleDistribution": {"type": "linear"},
            "hideFrom": {"legend": False, "tooltip": False, "viz": False},
            "thresholdsStyle": {"mode": "off"},
        },
        "mappings": [], "unit": unit,
        "thresholds": {"mode": "absolute", "steps": [{"color": "text", "value": None}]},
    }
    if minv is not None:
        d["min"] = minv
    if maxv is not None:
        d["max"] = maxv
    if decimals is not None:
        d["decimals"] = decimals
    return {
        "type": "timeseries", "title": title, "description": desc,
        "id": nid(), "datasource": DS,
        "gridPos": {"h": h, "w": w, "x": x, "y": y},
        "fieldConfig": {"defaults": d, "overrides": overrides or []},
        "options": {
            # A legend is always present for >=2 series and carries values, so
            # identity is never colour-alone.
            "legend": {"displayMode": "table", "placement": "bottom",
                       "showLegend": True, "calcs": ["lastNotNull", "mean"]},
            "tooltip": {"mode": "multi", "sort": "none"},
        },
        "targets": targets,
    }


def stat(title, y, x, w, h, q, unit="", steps=None, mappings=None, desc="",
         decimals=2, calc="lastNotNull", textsize=42):
    return {
        "type": "stat", "title": title, "description": desc,
        "id": nid(), "datasource": DS,
        "gridPos": {"h": h, "w": w, "x": x, "y": y},
        "fieldConfig": {"defaults": {
            "color": {"mode": "thresholds"},
            "mappings": mappings or [],
            "unit": unit, "decimals": decimals,
            "thresholds": {"mode": "absolute",
                           "steps": steps or [{"color": "text", "value": None}]},
        }, "overrides": []},
        "options": {
            "reduceOptions": {"calcs": [calc], "fields": "", "values": False},
            "orientation": "auto", "textMode": "auto", "colorMode": "value",
            "graphMode": "area", "justifyMode": "auto",
            "text": {"valueSize": textsize},
        },
        "targets": [target(q)],
    }


# MOS threshold bands, matching the tool's own published state machine
# (bad < 1.80 <= poor < 2.60 <= fair < 3.40 <= good/excellent).
MOS_STEPS = [
    {"color": CRIT, "value": None},
    {"color": SERIOUS, "value": 1.8},
    {"color": WARN, "value": 2.6},
    {"color": GOOD, "value": 3.4},
]

# ------------------------------------------------------------------ Overview
row("Overview", 0)
y = 1
panels.append(stat(
    "MOS — overall", y, 0, 4, 5,
    flux("qoe", ["mos_overall_smoothed"]) + '\n  |> yield(name: "mos")',
    steps=MOS_STEPS, decimals=2,
    desc="Smoothed combined audio+video MOS. Bands match srtbench's own state machine. "
         "UNCALIBRATED by default: trustworthy as a relative indicator, not as an "
         "absolute opinion score."))
panels.append(stat(
    "Video MOS", y, 4, 4, 5,
    flux("qoe", ["mos_video"]) + '\n  |> yield(name: "v")', steps=MOS_STEPS))
panels.append(stat(
    "Audio MOS", y, 8, 4, 5,
    flux("qoe", ["mos_audio"]) + '\n  |> yield(name: "a")', steps=MOS_STEPS,
    desc="Absent, not zero, when the stream carries no audio track."))
panels.append(stat(
    "Residual video loss", y, 12, 4, 5,
    flux("qoe", ["effective_loss_pct"]) + '\n  |> yield(name: "l")',
    unit="percent", decimals=3,
    steps=[{"color": GOOD, "value": None}, {"color": WARN, "value": 0.1},
           {"color": SERIOUS, "value": 0.5}, {"color": CRIT, "value": 1.0}],
    desc="Post-ARQ loss that SRT abandoned past its deadline. This is what actually "
         "damages the picture, not raw SRT loss, most of which is recovered."))
panels.append(stat(
    "RTT", y, 16, 4, 5,
    flux("srt_transport", ["ms_rtt"]) + '\n  |> yield(name: "rtt")',
    unit="ms", decimals=1,
    steps=[{"color": GOOD, "value": None}, {"color": WARN, "value": 100},
           {"color": SERIOUS, "value": 250}, {"color": CRIT, "value": 500}]))
panels.append(stat(
    "Model provenance", y, 20, 4, 5,
    flux("qoe", ["calibrated"], fn="last", extra=BOOL_TO_INT) + '\n  |> yield(name: "cal")',
    decimals=0, textsize=22,
    mappings=[{"type": "value", "options": {
        "0": {"text": "UNCALIBRATED", "color": WARN, "index": 0},
        "false": {"text": "UNCALIBRATED", "color": WARN, "index": 1},
        "1": {"text": "calibrated", "color": GOOD, "index": 2},
        "true": {"text": "calibrated", "color": GOOD, "index": 3}}}],
    steps=[{"color": WARN, "value": None}],
    desc="Whether the coefficient profile has been fitted against ground truth. The "
         "shipped defaults are derived estimates, so this reads UNCALIBRATED."))

# ------------------------------------------------------------------- Quality
row("Quality", 6)
y = 7
panels.append(ts(
    "MOS over time — parametric estimate vs VMAF ground truth", y, 0, 16, 11,
    [target(flux("qoe", ["mos_overall", "mos_video", "mos_audio"]) +
            '\n  |> yield(name: "mos")', "A"),
     target(flux("srtbench_ref", ["mos_vmaf"]) + '\n  |> yield(name: "ref")', "B")],
    unit="", minv=1, maxv=5, decimals=2,
    desc="Lines are the 1 Hz parametric model. Violet markers are the duty-cycled "
         "full-reference VMAF mapped onto the MOS scale. The distance between them "
         "is the calibration error.",
    overrides=color_overrides([
        ("mos_overall", BLUE, [{"id": "custom.lineWidth", "value": 3}]),
        ("mos_video", ORANGE, []),
        ("mos_audio", AQUA, []),
        # Ground truth is sparse and categorically different from the estimate,
        # so it is drawn as discrete markers rather than a line that would imply
        # a continuity it does not have.
        ("mos_vmaf", VIOLET, [
            {"id": "custom.drawStyle", "value": "points"},
            {"id": "custom.pointSize", "value": 10},
            {"id": "displayName", "value": "mos_vmaf (ground truth)"}]),
    ])))
panels.append(ts(
    "VMAF (raw)", y, 16, 8, 11,
    [target(flux("srtbench_ref", ["vmaf"]) + '\n  |> yield(name: "vmaf")')],
    unit="", minv=0, maxv=100, decimals=1,
    desc="Full-reference video quality, 0-100. Sparse by design: it runs on a duty cycle.",
    overrides=color_overrides([("vmaf", VIOLET, [
        {"id": "custom.showPoints", "value": "always"},
        {"id": "custom.pointSize", "value": 8}])])))

# --------------------------------------------------------- Loss & transport
row("Loss & transport", 18)
y = 19
panels.append(ts(
    "Effective loss, per stream", y, 0, 8, 8,
    [target(flux("qoe", ["effective_loss_pct", "audio_loss_pct"]) +
            '\n  |> yield(name: "loss")')],
    unit="percent", minv=0, decimals=3,
    desc="Damage that reached the decoder, attributed per PID. Video and audio are "
         "measured separately because a muxed stream carries roughly 12x more video "
         "packets than audio, so one stream-wide figure would misattribute the damage.",
    overrides=color_overrides([
        ("effective_loss_pct", ORANGE, [{"id": "displayName", "value": "video loss %"}]),
        ("audio_loss_pct", AQUA, [{"id": "displayName", "value": "audio loss % (5s window)"}]),
    ])))
panels.append(ts(
    "SRT packet outcomes", y, 8, 8, 8,
    [target(flux("srt_transport", ["pkt_recv_loss", "pkt_recv_retrans", "pkt_recv_drop"]) +
            '\n  |> yield(name: "pkts")')],
    unit="none", minv=0, decimals=2, axis="packets/s",
    desc="loss = initially missing, mostly recovered. retrans = recovered by ARQ. "
         "drop = abandoned past the deadline, and the only one that damages the picture.",
    overrides=color_overrides([
        ("pkt_recv_loss", BLUE, []),
        ("pkt_recv_retrans", VIOLET, []),
        ("pkt_recv_drop", MAGENTA, [{"id": "custom.lineWidth", "value": 3}]),
    ])))
panels.append(ts(
    "Round-trip time", y, 16, 8, 8,
    [target(flux("srt_transport", ["ms_rtt"]) + '\n  |> yield(name: "rtt")')],
    unit="ms", minv=0, decimals=1,
    overrides=color_overrides([("ms_rtt", BLUE, [])])))

y = 27
panels.append(ts(
    "Bitrate", y, 0, 12, 8,
    [target(flux("qoe", ["br_kbps", "br_kbps_received"]) + '\n  |> yield(name: "br")')],
    unit="Kbits", minv=0, decimals=1,
    desc="Offered vs received. Under loss the offered figure is reconstructed so the "
         "loss is not charged twice, once through a lower bitrate and again through "
         "the loss term.",
    overrides=color_overrides([
        ("br_kbps", BLUE, [{"id": "displayName", "value": "offered (loss-corrected)"}]),
        ("br_kbps_received", MAGENTA, [{"id": "displayName", "value": "received"}]),
    ])))
panels.append(ts(
    "Receiver buffer & latency budget", y, 12, 12, 8,
    [target(flux("srt_transport", ["ms_recv_buf", "ms_recv_tsbpd_delay"]) +
            '\n  |> yield(name: "buf")')],
    unit="ms", minv=0, decimals=0,
    desc="Buffer occupancy against the configured TSBPD budget. As occupancy "
         "approaches the budget, packets start being abandoned.",
    overrides=color_overrides([
        ("ms_recv_buf", BLUE, []),
        ("ms_recv_tsbpd_delay", VIOLET, [
            {"id": "custom.lineStyle", "value": {"fill": "dash", "dash": [10, 10]}},
            {"id": "displayName", "value": "TSBPD budget"}]),
    ])))

# --------------------------------------------------- Stream & decode health
row("Stream & decode health", 35)
y = 36
panels.append(ts(
    "MPEG-TS continuity errors, per PID", y, 0, 8, 8,
    [target(flux("ts_stream", ["cc_lost_video", "cc_lost_audio"]) + '\n  |> yield(name: "cc")')],
    unit="none", minv=0, decimals=2, axis="packets/s",
    desc="Packets the decoder never received, counted exactly and separately per PID. "
         "This is the authoritative loss source; SRT drops only correct its magnitude.",
    overrides=color_overrides([
        ("cc_lost_video", ORANGE, []), ("cc_lost_audio", AQUA, [])])))
panels.append(ts(
    "Video freezes", y, 8, 8, 8,
    [target(flux("media_video", ["freeze_ms", "freeze_ms_max"]) + '\n  |> yield(name: "fz")')],
    unit="ms", minv=0, decimals=0,
    desc="Freezes dominate perceived quality on a real uplink, and G.1070 has no term "
         "for them, so srtbench adds a stall penalty on top.",
    overrides=color_overrides([
        ("freeze_ms", ORANGE, []),
        ("freeze_ms_max", MAGENTA, [
            {"id": "custom.lineStyle", "value": {"fill": "dash", "dash": [10, 10]}}])])))
panels.append(ts(
    "Decode errors", y, 16, 8, 8,
    [target(flux("media_video", ["decode_errors", "corrupt_frames"]) +
            '\n  |> yield(name: "dec")')],
    unit="", minv=0, decimals=2,
    desc="Corroboration only, never part of the loss arithmetic. Errors here with zero "
         "measured loss mean srtbench is under-counting, and it raises a floor in response.",
    overrides=color_overrides([
        ("decode_errors", ORANGE, []), ("corrupt_frames", MAGENTA, [])])))

y = 44
panels.append(ts(
    "A/V sync drift", y, 0, 8, 8,
    [target(flux("av_sync", ["drift_ms"]) + '\n  |> yield(name: "drift")')],
    unit="ms", decimals=1,
    desc="Deviation from the stream's OWN baseline, not the raw A/V offset. MPEG-TS "
         "muxers run video ahead of audio by design (measured: -242 ms on a healthy "
         "stream), so penalising the raw offset would fire on every stream. Positive "
         "means audio leads. Tolerance is +40/-60 ms.",
    overrides=color_overrides([("drift_ms", BLUE, [])])))
panels.append(ts(
    "PCR jitter", y, 8, 8, 8,
    [target(flux("ts_stream", ["pcr_jitter_ms"]) + '\n  |> yield(name: "pcr")')],
    unit="ms", minv=0, decimals=2,
    desc="Spread of arrival time against the program clock. Measured against arrival, "
         "not between consecutive PCR values, which are perfect by construction.",
    overrides=color_overrides([("pcr_jitter_ms", BLUE, [])])))
panels.append(ts(
    "Audio dropouts & silence", y, 16, 8, 8,
    [target(flux("media_audio", ["gap_ms", "silence_ms"]) + '\n  |> yield(name: "au")')],
    unit="ms", minv=0, decimals=0,
    desc="Silence is DIAGNOSTIC only: a streamer who stops talking is not a defect. It "
         "becomes a scored gap only when the frame count corroborates dead audio.",
    overrides=color_overrides([
        ("gap_ms", AQUA, []),
        ("silence_ms", MAGENTA, [
            {"id": "custom.lineStyle", "value": {"fill": "dash", "dash": [10, 10]}},
            {"id": "displayName", "value": "silence (diagnostic)"}])])))

# ------------------------------------------- Diagnostics & model internals
row("Diagnostics & model internals", 52)
y = 53
panels.append(ts(
    "Impairment multipliers", y, 0, 8, 8,
    [target(flux("qoe", ["k_freeze", "k_sync", "k_audio_gap"]) + '\n  |> yield(name: "k")')],
    unit="", minv=0, maxv=1, decimals=3,
    desc="Each multiplies the MOS headroom, so a score can never fall below 1.0. "
         "A value of 1.0 means no penalty.",
    overrides=color_overrides([
        ("k_freeze", ORANGE, []), ("k_audio_gap", AQUA, []), ("k_sync", BLUE, [])])))
panels.append(ts(
    "Measurement confidence", y, 8, 8, 8,
    [target(flux("qoe", ["confidence", "loss_agreement"]) + '\n  |> yield(name: "conf")')],
    unit="", minv=0, maxv=1, decimals=2,
    desc="loss_agreement of 0.3 means the decoder complained while the parser saw no "
         "loss. That is the dangerous direction, because it means under-counting.",
    overrides=color_overrides([
        ("confidence", BLUE, []), ("loss_agreement", VIOLET, [])])))
panels.append({
    "type": "table", "title": "Unscored windows",
    "description": "Windows srtbench declined to score, and why. An unscoreable window "
                   "is never written as MOS 1.0; it carries a reason instead.",
    "id": nid(), "datasource": DS,
    "gridPos": {"h": 8, "w": 8, "x": 16, "y": y},
    "fieldConfig": {"defaults": {
        "custom": {"align": "auto", "filterable": True},
        "mappings": [],
        "thresholds": {"mode": "absolute", "steps": [{"color": "text", "value": None}]},
    }, "overrides": []},
    "options": {"showHeader": True, "sortBy": [{"desc": True, "displayName": "Time"}]},
    "targets": [target(
        'from(bucket: "${bucket}")\n'
        "  |> range(start: v.timeRangeStart, stop: v.timeRangeStop)\n"
        '  |> filter(fn: (r) => r._measurement == "qoe" and r._field == "reason")\n'
        "  |> filter(fn: (r) => %s)\n"
        '  |> filter(fn: (r) => r._value != "")\n'
        '  |> keep(columns: ["_time", "_value", "session_id"])\n'
        "  |> rename(columns: {_value: \"reason\"})\n"
        '  |> sort(columns: ["_time"], desc: true)\n'
        "  |> limit(n: 200)" % SESSION)],
})

dash = {
    "__inputs": [{
        "name": "DS_INFLUXDB",
        "label": "InfluxDB",
        "description": "InfluxDB 2.x datasource, configured with Flux as the query language.",
        "type": "datasource",
        "pluginId": "influxdb",
        "pluginName": "InfluxDB",
    }],
    "__requires": [
        {"type": "grafana", "id": "grafana", "name": "Grafana", "version": "10.0.0"},
        {"type": "datasource", "id": "influxdb", "name": "InfluxDB", "version": "1.0.0"},
        {"type": "panel", "id": "timeseries", "name": "Time series", "version": ""},
        {"type": "panel", "id": "stat", "name": "Stat", "version": ""},
        {"type": "panel", "id": "table", "name": "Table", "version": ""},
    ],
    "annotations": {"list": [{
        "builtIn": 1, "datasource": {"type": "grafana", "uid": "-- Grafana --"},
        "enable": True, "hide": True, "iconColor": "rgba(0, 211, 255, 1)",
        "name": "Annotations & Alerts", "type": "dashboard"}]},
    "description": "SRT stream quality: MOS for video, audio and combined, with the "
                   "transport and decode telemetry that explains it. Emitted by srtbench.",
    "editable": True,
    "fiscalYearStartMonth": 0,
    # Shared crosshair: every panel is the same timeline, and reading them
    # together is the point.
    "graphTooltip": 1,
    "links": [],
    "panels": panels,
    "preload": False,
    "refresh": "10s",
    "schemaVersion": 39,
    "tags": ["srtbench", "srt", "mos", "video", "audio"],
    "templating": {"list": [
        {"type": "textbox", "name": "bucket", "label": "Bucket",
         "query": "srtbench", "current": {"text": "srtbench", "value": "srtbench"},
         "options": [], "hide": 0, "skipUrlSync": False,
         "description": "The InfluxDB 2.x bucket srtbench writes to."},
        {"type": "query", "name": "session", "label": "Session",
         "datasource": DS, "refresh": 2, "hide": 0,
         "includeAll": True, "multi": True, "allValue": ".*",
         "current": {"text": ["All"], "value": ["$__all"]},
         "options": [], "regex": "", "sort": 5, "skipUrlSync": False,
         "definition": "session_id",
         "query": 'import "influxdata/influxdb/schema"\n'
                  'schema.tagValues(bucket: "${bucket}", tag: "session_id")'},
    ]},
    "time": {"from": "now-15m", "to": "now"},
    "timepicker": {"refresh_intervals": ["5s", "10s", "30s", "1m", "5m"]},
    "timezone": "browser",
    "title": "srtbench — SRT stream quality (MOS)",
    "uid": "srtbench-mos",
    "version": 1,
    "weekStart": "",
}

out = "grafana/srtbench-dashboard.json"
with open(out, "w", encoding="utf-8") as f:
    json.dump(dash, f, indent=2, ensure_ascii=False)
    f.write("\n")

print("wrote", out)
print("panels:", sum(1 for p in panels if p["type"] != "row"),
      "| rows:", sum(1 for p in panels if p["type"] == "row"))
