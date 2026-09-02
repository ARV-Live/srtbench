// Command srtbench streams test media over SRT, measures the received stream,
// and reports a MOS score with supporting telemetry.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/ARV-Live/srtbench/internal/calib"
	"github.com/ARV-Live/srtbench/internal/config"
	"github.com/ARV-Live/srtbench/internal/qoe"
	"github.com/ARV-Live/srtbench/internal/sink"
	"github.com/ARV-Live/srtbench/internal/source"
)

// loadProfile resolves the coefficient set: a fitted file when given, else a
// shipped one by name.
func loadProfile(cfg config.Config) (qoe.Profile, error) {
	if cfg.QoE.ProfilePath != "" {
		return qoe.LoadProfileFile(cfg.QoE.ProfilePath)
	}
	return qoe.LoadProfile(cfg.QoE.Profile)
}

// version is stamped at build time with -ldflags "-X main.version=...".
// It defaults to "dev" so a locally built binary never claims to be a release.
var version = "dev"

const usage = `srtbench - SRT stream quality benchmarking with MOS scoring

Usage:
  srtbench receive [flags]    listen for SRT, measure, score, publish
  srtbench send    [flags]    generate test A/V and push it over SRT
  srtbench run     [flags]    both, wired locally (one-command demo)
  srtbench sweep   [flags]    walk an impairment grid, collecting fit data
  srtbench calibrate [flags]  fit a profile against the collected ground truth
  srtbench profiles           list coefficient profiles and their provenance
  srtbench version            print the version

Common flags:
  -config PATH        YAML config file
  -endpoint URL       srt://host:port?streamid=...&latency=...
  -mode MODE          caller | listener
  -csv PATH           write measurements to CSV ("-" for stdout)
  -influx-url URL     InfluxDB 2.x endpoint (also SRTBENCH_INFLUX_*)
  -profile NAME       coefficient profile (default h264-1080p)
  -profile-file PATH  a fitted profile written by 'srtbench calibrate'
  -duration SECONDS   stop after this long
  -streamid ID        stream id for this direction

Round-tripping a real ingest (this is what makes run work against a server):
  -publish-streamid ID   the sender pushes with this id
  -read-streamid ID      the receiver pulls the same stream back with this id
  With both set, run sends to the endpoint and reads the stream back from it,
  so the score covers the whole path through your server rather than a local
  socket. For MediaMTX the ids are "publish:<key>" and "read:<key>".

Sender flags:
  -bitrate KBPS       video bitrate            -fps N
  -size WxH           resolution               -vcodec libx264|libx265
  -input PATH         stream a media file instead of the test pattern. Its own
                      audio is used when it has any; the synthetic 1 kHz tone
                      stands in when it does not, so the audio half of the
                      score applies either way.
  -no-audio           stream video only
  -impair-loss PCT    drop this % of packets before sending
  -impair-burst N     drop in runs of N (mobile links fail in bursts)
  -impair-delay DUR   add latency

Sweep flags:
  -sweep-bitrates LIST   kbps, comma separated   (default 800,1500,3000,5000)
  -sweep-fps LIST        frame rates             (default 30)
  -sweep-loss LIST       injected loss percents  (default 0,0.5,2,5)
  -sweep-latency LIST    SRT latencies           (default 200ms,1s)
  -sweep-seconds N       seconds per cell        (default 45)

Calibrate flags:
  -in FILE            the sweep's qoe CSV        -ref FILE  its .ref.csv
  -out FILE           profile to write           -refit-video  also refit the
  -lambda N           pull toward defaults                     loss block

Settings resolve flags > SRTBENCH_* env > YAML > defaults.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	cmd := os.Args[1]
	args := os.Args[2:]

	switch cmd {
	case "profiles":
		listProfiles()
		return
	case "version", "-version", "--version":
		fmt.Printf("srtbench %s\n", version)
		return
	}

	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, usage) }

	var (
		cfgPath   = fs.String("config", "", "YAML config file")
		endpoint  = fs.String("endpoint", "", "srt:// endpoint")
		mode      = fs.String("mode", "", "caller|listener")
		csvPath   = fs.String("csv", "", "CSV output path, - for stdout")
		influxURL = fs.String("influx-url", "", "InfluxDB 2.x URL")
		influxTok = fs.String("influx-token", "", "InfluxDB token")
		influxOrg = fs.String("influx-org", "", "InfluxDB org")
		influxBkt = fs.String("influx-bucket", "", "InfluxDB bucket")
		streamID  = fs.String("streamid", "", "stream id for this direction")
		pubID     = fs.String("publish-streamid", "", "stream id the sender publishes with")
		readID    = fs.String("read-streamid", "", "stream id the receiver reads with")
		profile   = fs.String("profile", "", "coefficient profile")
		duration  = fs.Int("duration", 0, "stop after N seconds")
		bitrate   = fs.Int("bitrate", 0, "video bitrate kbps")
		fps       = fs.Int("fps", 0, "frame rate")
		size      = fs.String("size", "", "WxH")
		vcodec    = fs.String("vcodec", "", "libx264|libx265")
		input     = fs.String("input", "", "media file instead of test pattern")
		noAudio   = fs.Bool("no-audio", false, "stream video only")
		impLoss   = fs.Float64("impair-loss", 0, "inject packet loss, percent")
		impBurst  = fs.Int("impair-burst", 1, "drop in bursts of N packets")
		impDelay  = fs.Duration("impair-delay", 0, "inject latency")
		verbose   = fs.Bool("v", false, "print each window to stderr")
		profFile  = fs.String("profile-file", "", "fitted profile written by calibrate")

		swBitrates = fs.String("sweep-bitrates", "800,1500,3000,5000", "kbps list")
		swFPS      = fs.String("sweep-fps", "30", "frame-rate list")
		swLoss     = fs.String("sweep-loss", "0,0.5,2,5", "injected loss percents")
		swLatency  = fs.String("sweep-latency", "200ms,1s", "SRT latency list")
		swBurst    = fs.Int("sweep-burst", 3, "burst length for injected loss")
		swSeconds  = fs.Int("sweep-seconds", 45, "seconds per sweep cell")
		swPort     = fs.Int("sweep-port", 9700, "first local port for sweep cells")

		calIn     = fs.String("in", "", "sweep qoe CSV")
		calRef    = fs.String("ref", "", "sweep reference CSV (default: <in>.ref.csv)")
		calOut    = fs.String("out", "profile-fitted.yaml", "profile to write")
		calRefit  = fs.Bool("refit-video", false, "also refit the video loss block")
		calLambda = fs.Float64("lambda", 2.0, "ridge pull toward the shipped defaults")
		calWindow = fs.Duration("ref-window", 4*time.Second, "reference segment length")
	)
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fatal(err)
	}
	// Flags win over env and file.
	setIf(endpoint, &cfg.SRT.Endpoint)
	setIf(mode, &cfg.SRT.Mode)
	setIf(csvPath, &cfg.Influx.CSV)
	setIf(influxURL, &cfg.Influx.URL)
	setIf(influxTok, &cfg.Influx.Token)
	setIf(influxOrg, &cfg.Influx.Org)
	setIf(influxBkt, &cfg.Influx.Bucket)
	setIf(streamID, &cfg.SRT.StreamID)
	setIf(pubID, &cfg.SRT.PublishStreamID)
	setIf(readID, &cfg.SRT.ReadStreamID)
	setIf(profile, &cfg.QoE.Profile)
	setIf(profFile, &cfg.QoE.ProfilePath)
	setIf(vcodec, &cfg.Media.VideoCodec)
	setIf(input, &cfg.Media.Input)
	if *bitrate > 0 {
		cfg.Media.VideoKbps = *bitrate
	}
	if *fps > 0 {
		cfg.Media.FPS = *fps
	}
	if *noAudio {
		cfg.Media.NoAudio = true
	}
	if *size != "" {
		if w, h, ok := parseSize(*size); ok {
			cfg.Media.Width, cfg.Media.Height = w, h
		} else {
			fatal(fmt.Errorf("bad -size %q, want WxH", *size))
		}
	}
	cfg.Impair.LossPct = *impLoss
	cfg.Impair.BurstLen = *impBurst
	cfg.Impair.Delay = *impDelay

	if cfg.Session.ID == "" {
		cfg.Session.ID = strconv.FormatInt(time.Now().Unix(), 36)
	}
	if err := resolveInput(cmd, &cfg); err != nil {
		fatal(err)
	}
	// The sender publishes nothing on its own, so it needs no output
	// configured; only the measuring side does.
	if cmd == "receive" || cmd == "run" || cmd == "sweep" {
		if err := cfg.Validate(); err != nil {
			fatal(err)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if *duration > 0 {
		var stop context.CancelFunc
		ctx, stop = context.WithTimeout(ctx, time.Duration(*duration)*time.Second)
		defer stop()
	}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	go func() { <-sig; cancel() }()

	switch cmd {
	case "calibrate":
		if *calIn == "" {
			fatal(fmt.Errorf("calibrate needs -in <sweep csv> (produced by `srtbench sweep -csv <file>`)"))
		}
		o := calib.DefaultOptions()
		o.RefitVideo = *calRefit
		o.Lambda = *calLambda
		err = runCalibrate(*calIn, *calRef, *calOut, cfg.QoE.Profile, *calWindow, o)
	case "sweep":
		bitrates, e1 := parseInts(*swBitrates)
		rates, e2 := parseInts(*swFPS)
		losses, e3 := parseFloats(*swLoss)
		lats, e4 := parseDurations(*swLatency)
		for _, e := range []error{e1, e2, e3, e4} {
			if e != nil {
				fatal(fmt.Errorf("sweep grid: %w", e))
			}
		}
		grid := buildGrid(bitrates, rates, losses, *swBurst, lats)
		prof, perr := loadProfile(cfg)
		if perr != nil {
			fatal(perr)
		}
		so := sweepOptions{
			Cells:    grid,
			CellDur:  time.Duration(*swSeconds) * time.Second,
			BasePort: *swPort, Verbose: *verbose,
		}
		fmt.Fprint(os.Stderr, describeSweep(so, prof))
		err = runSweep(ctx, cfg, so)
	case "receive":
		err = runReceive(ctx, cfg, *verbose)
	case "send":
		err = runSend(ctx, cfg)
	case "run":
		err = runBoth(ctx, cfg, *verbose)
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	// Report real failures even when the context has ended. The previous
	// `ctx.Err() == nil` guard silenced anything that failed during a timed
	// run, which is exactly when a connection failure happens.
	if err != nil && !errors.Is(err, context.Canceled) &&
		!errors.Is(err, context.DeadlineExceeded) {
		fatal(err)
	}
}

func listProfiles() {
	ps := qoe.Profiles()
	fmt.Println("Coefficient profiles. Provenance is per block, and it matters:")
	fmt.Println("an uncalibrated score is a reliable RELATIVE indicator, not an")
	fmt.Println("absolute opinion score. Run 'srtbench calibrate' to change that.")
	fmt.Println()
	fmt.Printf("%-18s %-11s %-11s %-11s %s\n", "NAME", "VIDEO", "AUDIO", "STALL", "CALIBRATED")
	for _, name := range []string{
		"h264-1080p", "h264-720p", "h265-1080p", "h265-720p",
		"h264-1080p-opus", "h265-1080p-opus",
	} {
		p, ok := ps[name]
		if !ok {
			continue
		}
		fmt.Printf("%-18s %-11s %-11s %-11s %v\n", p.Name,
			p.Provenance.Video, p.Provenance.Audio, p.Provenance.Stall,
			p.Provenance.Calibrated())
	}
}

// buildSink assembles the configured outputs. An offline CSV sink is always
// available so the tool works with no database at all.
func buildSink(cfg config.Config) (sink.Sink, error) {
	var sinks []sink.Sink
	if cfg.Influx.URL != "" {
		s, err := sink.NewInflux(sink.InfluxConfig{
			URL: cfg.Influx.URL, Token: cfg.Influx.Token,
			Org: cfg.Influx.Org, Bucket: cfg.Influx.Bucket,
		})
		if err != nil {
			return nil, err
		}
		sinks = append(sinks, s)
	}
	if cfg.Influx.CSV != "" {
		s, err := sink.NewCSV(cfg.Influx.CSV, "qoe")
		if err != nil {
			return nil, err
		}
		sinks = append(sinks, s)

		// The full-reference series goes to a companion file. It is sparse and
		// has different columns, so folding it into the same CSV would leave
		// most rows mostly empty -- and calibration needs both halves.
		if ref := refCSVPath(cfg.Influx.CSV); ref != "" {
			r, err := sink.NewCSV(ref, "srtbench_ref")
			if err != nil {
				return nil, err
			}
			sinks = append(sinks, r)
		}
	}
	if len(sinks) == 0 {
		return sink.Discard{}, nil
	}
	return sink.Multi(sinks...), nil
}

// refCSVPath is the companion file for the reference series. Stdout gets none:
// two interleaved schemas on one stream would be unparseable.
func refCSVPath(path string) string {
	if path == "" || path == "-" {
		return ""
	}
	if i := strings.LastIndex(path, "."); i > 0 {
		return path[:i] + ".ref" + path[i:]
	}
	return path + ".ref.csv"
}

func setIf(from *string, to *string) {
	if from != nil && *from != "" {
		*to = *from
	}
}

func parseSize(s string) (int, int, bool) {
	var w, h int
	if _, err := fmt.Sscanf(s, "%dx%d", &w, &h); err != nil || w <= 0 || h <= 0 {
		return 0, 0, false
	}
	return w, h, true
}

// resolveInput probes an -input file and records what it actually contains.
//
// This runs before anything is started because ffmpeg will not complain about
// a track that is simply absent. A video-only file used with -input produced a
// video-only stream while the configuration still said audio was on: the audio
// half of the MOS reported "no track" for the whole run, and nothing anywhere
// explained why. Knowing up front lets the sender substitute the synthetic tone
// and say so, and lets the reference pass compare against whatever is actually
// on the wire.
func resolveInput(cmd string, cfg *config.Config) error {
	if cfg.Media.Input == "" {
		return nil
	}
	sends := cmd == "send" || cmd == "run" || cmd == "sweep"

	tracks, err := source.Inspect(context.Background(), cfg.Media.Input)
	if err != nil {
		if sends {
			return fmt.Errorf("-input %q cannot be read: %w", cfg.Media.Input, err)
		}
		// The receiver only needs the file as a reference. Losing it costs the
		// ground truth, not the run -- but silence here would leave a dashboard
		// with no reference and no reason.
		fmt.Fprintf(os.Stderr,
			"srtbench: full-reference scoring is off - the -input file cannot be read here (%v).\n"+
				"  The parametric MOS is unaffected. Ground truth needs the same file "+
				"reachable from both ends.\n", err)
		cfg.QoE.Reference = false
		return nil
	}
	if sends && !tracks.HasVideo {
		return fmt.Errorf("-input %q has no video track", cfg.Media.Input)
	}
	cfg.Media.InputHasAudio = tracks.HasAudio

	if !cfg.Media.NoAudio && !tracks.HasAudio && sends {
		fmt.Fprintf(os.Stderr,
			"srtbench: -input %s has no audio track; sending the synthetic 1 kHz tone "+
				"so the audio half of the score still applies.\n"+
				"  Pass -no-audio to stream video only instead.\n", cfg.Media.Input)
	}
	return nil
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "srtbench:", err)
	os.Exit(1)
}
