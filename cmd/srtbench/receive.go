package main

import (
	"context"
	"fmt"
	"math"
	"os"
	"time"

	gosrt "github.com/datarhei/gosrt"

	"github.com/ARV-Live/srtbench/internal/config"
	"github.com/ARV-Live/srtbench/internal/probe"
	"github.com/ARV-Live/srtbench/internal/qoe"
	"github.com/ARV-Live/srtbench/internal/sink"
	"github.com/ARV-Live/srtbench/internal/srt"
	"github.com/ARV-Live/srtbench/internal/ts"
)

// runReceive is the measuring end: accept SRT, parse, decode, score, publish.
func runReceive(ctx context.Context, cfg config.Config, verbose bool) error {
	return runReceiveSink(ctx, cfg, verbose, nil)
}

// runReceiveSink is runReceive with a caller-supplied output.
//
// A sweep needs ONE sink spanning every cell: a per-cell sink would truncate
// the file at each new cell and leave only the last one, and calibration would
// then see a single cell and refuse to fit.
func runReceiveSink(ctx context.Context, cfg config.Config, verbose bool, out sink.Sink) error {
	opts, err := srt.ParseURL(cfg.SRT.Endpoint)
	if err != nil {
		return err
	}
	if cfg.SRT.Mode != "" {
		opts.Mode = srt.Mode(cfg.SRT.Mode)
	}
	if cfg.SRT.Latency > 0 {
		opts.Latency = cfg.SRT.Latency
	}
	if id := cfg.SRT.RecvStreamID(); id != "" {
		opts.StreamID = id
	}
	opts.Passphrase = cfg.SRT.Passphrase

	prof, err := loadProfile(cfg)
	if err != nil {
		return err
	}
	if out == nil {
		built, err := buildSink(cfg)
		if err != nil {
			return err
		}
		defer built.Close()
		out = built
	}

	var conn gosrt.Conn
	if opts.Mode == srt.ModeListener {
		l, err := srt.Listen(opts)
		if err != nil {
			// Not wrapped: Listen already names the address, and its
			// non-local diagnostic is a multi-line message that a prefix
			// would only make harder to read.
			return err
		}
		defer l.Close()
		fmt.Fprintf(os.Stderr, "srtbench: listening on srt://%s (profile %s)\n", opts.Addr, prof.Name)

		accepted := make(chan gosrt.Conn, 1)
		errc := make(chan error, 1)
		go func() {
			c, err := l.Accept()
			if err != nil {
				errc <- err
				return
			}
			accepted <- c
		}()
		select {
		case conn = <-accepted:
		case err := <-errc:
			return err
		case <-ctx.Done():
			return nil
		}
	} else {
		// Caller mode retries, because a server has nothing to hand out until
		// something is publishing. On a round trip our own sender is the
		// publisher and is still starting up, so the first attempts are
		// expected to fail and are not worth reporting.
		fmt.Fprintf(os.Stderr, "srtbench: connecting to srt://%s (streamid %q) ...\n",
			opts.Addr, opts.StreamID)
		var lastErr error
		deadline := time.Now().Add(20 * time.Second)
		for conn == nil {
			c, derr := srt.Dial(opts)
			if derr == nil {
				conn = c
				break
			}
			lastErr = derr
			if time.Now().After(deadline) {
				return fmt.Errorf("could not read from srt://%s (streamid %q): %w",
					opts.Addr, opts.StreamID, derr)
			}
			select {
			case <-ctx.Done():
				return fmt.Errorf("gave up reading from srt://%s (streamid %q): %w",
					opts.Addr, opts.StreamID, lastErr)
			case <-time.After(300 * time.Millisecond):
			}
		}
	}
	defer conn.Close()
	fmt.Fprintf(os.Stderr, "srtbench: connected, measuring every %s\n", cfg.QoE.Interval)

	return measure(ctx, cfg, prof, conn, out, verbose)
}

// measure runs the read loop and the scoring ticker.
func measure(ctx context.Context, cfg config.Config, prof qoe.Profile,
	conn gosrt.Conn, out sink.Sink, verbose bool) error {

	parser := ts.New()
	dec, err := probe.New(ctx, cfg.Media.FPS, !cfg.Media.NoAudio)
	if err != nil {
		return fmt.Errorf("start decoder: %w", err)
	}
	defer dec.Close()

	// The decoder is fed through a bounded, non-blocking channel.
	//
	// This is the most important structural decision in the receive path. If a
	// slow decoder were allowed to block the SRT reader, the receive buffer
	// would overflow and produce REAL PktRecvDrop -- the tool would then be
	// measuring an impairment it created itself. So we drop, count it, and
	// invalidate the affected window rather than quietly scoring our own bug
	// as network loss.
	feed := make(chan []byte, 256)
	var toolDrop uint64
	go func() {
		for b := range feed {
			dec.Write(b)
		}
	}()

	var ref *refRunner
	if cfg.QoE.Reference {
		if ok, why := referenceUsable(cfg); ok {
			ref = newRefRunner(cfg, prof, out, verbose)
		} else {
			// Silence here would be the dangerous option: the dashboard would
			// simply show no ground truth and nobody would know why.
			fmt.Fprintf(os.Stderr,
				"srtbench: full-reference scoring is off for this run - %s.\n"+
					"  The parametric MOS is unaffected. To get ground truth here, "+
					"stream a real file with -input <path> reachable from both ends.\n", why)
		}
	}

	start := time.Now()
	var connectedMs float64
	readErr := make(chan error, 1)
	go func() {
		buf := make([]byte, 64*1024)
		for {
			n, err := conn.Read(buf)
			if n > 0 {
				b := make([]byte, n)
				copy(b, buf[:n])
				parser.WriteAt(b, time.Since(start).Seconds())
				if ref != nil {
					ref.feed(b)
				}
				select {
				case feed <- b:
				default:
					toolDrop += uint64(n)
				}
			}
			if err != nil {
				readErr <- err
				return
			}
		}
	}()

	model := qoe.NewModel(prof)
	var sampler srt.Sampler
	audioAcc := newAudioAccumulator(prof.Audio.LossWindowMs)
	sm := newSmoother()

	// Warmup covers establishing deltas, the TSBPD buffer filling and the
	// decoder waiting for its first keyframe. At 3 s latency that is 5 windows.
	warmup := int(math.Max(5, math.Ceil((cfg.SRT.Latency.Seconds()*1000+2000)/
		float64(cfg.QoE.Interval.Milliseconds()))))

	tick := time.NewTicker(cfg.QoE.Interval)
	defer tick.Stop()
	last := time.Now()

	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-readErr:
			if err.Error() == "EOF" {
				return nil
			}
			fmt.Fprintf(os.Stderr, "srtbench: stream ended: %v\n", err)
			return nil
		case now := <-tick.C:
			windowMs := float64(now.Sub(last).Milliseconds())
			last = now

			net := sampler.Sample(conn)
			tsSnap := parser.Snapshot(windowMs / 1000)
			health := dec.Snapshot()

			// The connection is up for the whole window unless we were told
			// otherwise; a mid-window drop shows up as a read error instead.
			connectedMs = windowMs

			s := buildSample(cfg, windowMs, connectedMs, net, tsSnap, health, audioAcc)
			if warmup > 0 {
				s.WarmupRemaining = warmup
				warmup--
			}
			if toolDrop > 0 {
				s.ToolDropBytes = toolDrop
				fmt.Fprintf(os.Stderr,
					"srtbench: WARNING dropped %d bytes feeding the decoder; "+
						"window discarded (this is our bottleneck, not the network)\n", toolDrop)
				toolDrop = 0
			}

			if ref != nil {
				if pts, ok := parser.FirstVideoPTS(); ok {
					ref.setBase(pts)
				}
				ref.tick(ctx, now)
			}

			sc := model.Evaluate(s)
			state, smoothed := sm.push(sc)
			publish(out, cfg, prof, now, s, sc, state, smoothed)
			if verbose {
				printWindow(sc, s, state)
			}
		}
	}
}

// audioAccumulator keeps audio damage over a longer window than the scoring
// interval. At ~179 audio TS packets per second, a 1 s window quantises audio
// loss to 0.56% steps and one lost packet swings audio MOS by 0.42 -- the
// series would be bimodal and unreadable. Five seconds brings that to 0.065.
type audioAccumulator struct {
	windowMs float64
	pkts     []uint64
	gaps     []uint64
	events   []uint64
	perMs    float64
}

func newAudioAccumulator(windowMs float64) *audioAccumulator {
	if windowMs <= 0 {
		windowMs = 5000
	}
	return &audioAccumulator{windowMs: windowMs}
}

func (a *audioAccumulator) push(d ts.PIDDelta, intervalMs float64) qoe.PIDLoss {
	a.perMs = intervalMs
	n := int(math.Max(1, a.windowMs/math.Max(intervalMs, 1)))
	a.pkts = appendCap(a.pkts, d.Packets, n)
	a.gaps = appendCap(a.gaps, d.CCLost, n)
	a.events = appendCap(a.events, d.CCErrors, n)
	return qoe.PIDLoss{
		Present:     d.Present,
		PID:         d.PID,
		PktsDelta:   sum(a.pkts),
		CCGapPkts:   sum(a.gaps),
		CCErrEvents: sum(a.events),
	}
}

func appendCap(s []uint64, v uint64, n int) []uint64 {
	s = append(s, v)
	if len(s) > n {
		s = s[len(s)-n:]
	}
	return s
}

func sum(s []uint64) uint64 {
	var t uint64
	for _, v := range s {
		t += v
	}
	return t
}

// buildSample assembles one window from every source.
func buildSample(cfg config.Config, windowMs, connectedMs float64,
	net srt.Delta, t ts.Snapshot, h probe.Health, acc *audioAccumulator) qoe.Sample {

	s := qoe.Sample{
		WindowMs:     windowMs,
		CounterReset: net.Reset,
		Stream: qoe.StreamProfile{
			VideoCodec: cfg.Media.VideoCodec, AudioCodec: cfg.Media.AudioCodec,
			Width: cfg.Media.Width, Height: cfg.Media.Height,
			FrameRateCoded:    float64(cfg.Media.FPS),
			AudioSampleRateHz: 48000, AudioChannels: 2,
			// Track presence comes from the PMT via the TS parser, never from
			// packet counts: a one-second audio dropout is a defect, whereas a
			// video-only stream is a configuration, and conflating them makes
			// every video-only stream look broken.
			HasVideoTrack: t.Video.Present,
			HasAudioTrack: t.Audio.Present,
			PMTSeen:       t.Video.Present || t.Audio.Present,
		},
		Net: qoe.NetSample{
			PktRecvDelta:          net.PktRecv,
			PktRecvLossDelta:      net.PktRecvLoss,
			PktRecvRetransDelta:   net.PktRecvRetrans,
			PktRecvDropDelta:      net.PktRecvDrop,
			PktRecvUndecryptDelta: net.PktRecvUndecrypt,
			ByteRecvDelta:         net.ByteRecv,
			MsRTT:                 net.MsRTT,
			MbpsRecvRate:          net.MbpsRecvRate,
			MbpsLinkCapacity:      net.MbpsLinkCapacity,
			MsRecvTsbPdDelay:      net.MsRecvTsbPdDelay,
			MsRecvBuf:             net.MsRecvBuf,
			PktFlightSize:         net.PktFlightSize,
			ConnectedMs:           connectedMs,
			AvgPayloadBytes:       payloadOrDefault(net.AvgPayloadBytes),
		},
		TS: qoe.TSSample{
			PktsTotalDelta: t.Packets,
			SyncLossEvents: t.Desyncs,
			Video:          toPIDLoss(t.Video),
			Audio:          toPIDLoss(t.Audio),
			AudioWin:       acc.push(t.Audio, windowMs),
			AudioWinMs:     acc.windowMs,
			PCRJitterMS:    t.PCRJitterMS,
			HavePCRJitter:  t.HavePCRJitter,
			// Deliberately NOT t.Video.Bytes: that counts whole 188-byte TS
			// packets including their headers, which overstates the elementary
			// stream. Leaving these zero lets the model fall through to its
			// packets x184 estimate, which is the honest figure available
			// without full PES payload accounting.
		},
		Video: qoe.VideoSample{
			FramesOutDelta:    h.FramesOut,
			FramesDupDelta:    h.FramesDup,
			FramesDropDelta:   h.FramesDrop,
			DecodeErrDelta:    h.DecodeErr,
			CorruptFrameDelta: h.CorruptFrm,
			NoFrameDelta:      h.NoFrame,
			FreezeEvents:      h.FreezeEvents,
			FreezeMsTotal:     h.FreezeMsTotal,
			FreezeMsMax:       h.FreezeMsMax,
			MsSinceLastFreeze: h.MsSinceLastFreeze,
		},
		Audio: qoe.AudioSample{
			FramesOutDelta: h.AudioFrames,
			DecodeErrDelta: h.AudioErr,
			SilenceMsTotal: h.SilenceMs,
			MsSinceLastGap: math.Inf(1),
		},
	}
	if cfg.Media.FPS > 0 {
		s.TS.FramePeriodMs = 1000 / float64(cfg.Media.FPS)
	}
	// The sync term takes the DEVIATION from the stream's own baseline, never
	// the raw offset: MPEG-TS muxers deliberately run video ahead of audio, so
	// a healthy stream carries a large constant offset that would otherwise be
	// penalised on every single stream.
	if t.HaveAVDrift {
		s.Sync = qoe.SyncSample{Valid: true, SkewMs: t.AVDriftMS, Samples: t.AVDriftSamples}
	}
	return s
}

func toPIDLoss(d ts.PIDDelta) qoe.PIDLoss {
	return qoe.PIDLoss{
		Present: d.Present, PID: d.PID,
		PktsDelta: d.Packets, CCErrEvents: d.CCErrors, CCGapPkts: d.CCLost,
	}
}

// payloadOrDefault falls back to the standard 1316 (7 x 188) when no traffic
// has been measured yet.
func payloadOrDefault(v float64) float64 {
	if v > 0 {
		return v
	}
	return 1316
}
