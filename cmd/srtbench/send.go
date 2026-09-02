package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/ARV-Live/srtbench/internal/config"
	"github.com/ARV-Live/srtbench/internal/impair"
	"github.com/ARV-Live/srtbench/internal/source"
	"github.com/ARV-Live/srtbench/internal/srt"
)

// runSend generates test media and pushes it over SRT.
func runSend(ctx context.Context, cfg config.Config) error {
	opts, err := srt.ParseURL(cfg.SRT.Endpoint)
	if err != nil {
		return err
	}
	// The sender is a caller unless explicitly told otherwise, which is what
	// every real encoder does.
	opts.Mode = srt.ModeCaller
	if cfg.SRT.Mode == string(srt.ModeListener) {
		opts.Mode = srt.ModeListener
	}
	if cfg.SRT.Latency > 0 {
		opts.Latency = cfg.SRT.Latency
	}
	if id := cfg.SRT.SendStreamID(); id != "" {
		opts.StreamID = id
	}
	opts.Passphrase = cfg.SRT.Passphrase

	spec := source.Spec{
		Input: cfg.Media.Input,
		// Resolved at startup by probing the file. When it is false the tone
		// stands in, so the audio half of the score keeps applying instead of
		// silently disappearing.
		InputHasAudio: cfg.Media.InputHasAudio,
		Width:         cfg.Media.Width,
		Height:        cfg.Media.Height,
		FPS:           cfg.Media.FPS,
		VideoCodec:    cfg.Media.VideoCodec,
		VideoKbps:     cfg.Media.VideoKbps,
		AudioCodec:    cfg.Media.AudioCodec,
		AudioKbps:     cfg.Media.AudioKbps,
		NoAudio:       cfg.Media.NoAudio,
		GOP:           cfg.Media.GOP,
		Realtime:      true,
	}

	proc, err := source.Start(ctx, spec)
	if err != nil {
		return err
	}

	// Connect after the encoder is up, but retry: with `run` the receiver may
	// still be binding its port.
	fmt.Fprintf(os.Stderr, "srtbench: connecting to srt://%s ...\n", opts.Addr)
	var (
		conn     io.WriteCloser
		lastErr  error
		began    = time.Now()
		deadline = began.Add(10 * time.Second)
	)
	for conn == nil {
		c, err := srt.Dial(opts)
		if err == nil {
			conn = c
			break
		}
		lastErr = err
		if time.Now().After(deadline) {
			return fmt.Errorf("could not connect to srt://%s: %w", opts.Addr, err)
		}
		select {
		case <-ctx.Done():
			// Never having connected is a failure worth reporting, even though
			// the context ended. Returning nil here made the sender exit 0 in
			// silence whenever -duration was shorter than the dial deadline --
			// no output, no error, nothing to debug.
			return fmt.Errorf("gave up connecting to srt://%s after %s: %w",
				opts.Addr, time.Since(began).Round(time.Second), lastErr)
		case <-time.After(200 * time.Millisecond):
		}
	}
	defer conn.Close()

	var dst io.Writer = conn
	var imp *impair.Writer
	if cfg.Impair.LossPct > 0 || cfg.Impair.Delay > 0 {
		imp = impair.New(conn, impair.Config{
			LossPct:  cfg.Impair.LossPct,
			BurstLen: cfg.Impair.BurstLen,
			Delay:    cfg.Impair.Delay,
			Jitter:   cfg.Impair.JitterMs,
			Seed:     cfg.Impair.Seed,
		})
		dst = imp
		fmt.Fprintf(os.Stderr,
			"srtbench: injecting %.2f%% loss in bursts of %d\n",
			cfg.Impair.LossPct, max(cfg.Impair.BurstLen, 1))
	}

	fmt.Fprintf(os.Stderr, "srtbench: sending %dx%d@%d %s %dkbps to srt://%s\n",
		spec.Width, spec.Height, spec.FPS, spec.VideoCodec, spec.VideoKbps, opts.Addr)

	// SRT is message-oriented; writing in payload-sized chunks keeps whole TS
	// packets together so a single lost datagram damages a bounded, knowable
	// amount of the stream.
	buf := make([]byte, 1316)
	done := make(chan error, 1)
	go func() {
		for {
			n, err := io.ReadFull(proc.Out, buf)
			if n > 0 {
				if _, werr := dst.Write(buf[:n]); werr != nil {
					done <- werr
					return
				}
			}
			if err != nil {
				done <- err
				return
			}
		}
	}()

	select {
	case <-ctx.Done():
	case err := <-done:
		if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
			if msg := proc.Stderr(); msg != "" {
				return fmt.Errorf("%w (ffmpeg: %s)", err, msg)
			}
			return err
		}
	}
	if imp != nil {
		fmt.Fprintf(os.Stderr, "srtbench: sent %d packets, dropped %d\n", imp.Passed, imp.Dropped)
	}
	return nil
}

// runBoth drives both directions from one command.
//
// With a publish and a read stream id configured it round-trips a real server:
// the sender publishes to the endpoint and the receiver reads the same stream
// back, so the score covers the entire path through the server. Without them
// it wires a local listener to a local caller, which measures only srtbench
// talking to itself.
func runBoth(ctx context.Context, cfg config.Config, verbose bool) error {
	if cfg.SRT.RoundTrip() {
		return runRoundTrip(ctx, cfg, verbose)
	}
	// One id alone cannot round-trip, and silently falling back to loopback
	// would report a healthy score for a server that was never touched.
	if (cfg.SRT.PublishStreamID == "") != (cfg.SRT.ReadStreamID == "") {
		missing, have := "read_streamid", "publish_streamid"
		if cfg.SRT.PublishStreamID == "" {
			missing, have = "publish_streamid", "read_streamid"
		}
		return fmt.Errorf(
			"run needs both stream ids to round-trip a server: %s is set but %s is not.\n"+
				"Set both to push to the endpoint and read the same stream back:\n"+
				"    srtbench run -endpoint 'srt://%s' \\\n"+
				"        -publish-streamid 'publish:<key>' -read-streamid 'read:<key>'\n"+
				"Or clear both to run a local loopback instead.",
			have, missing, hostOf(cfg.SRT.Endpoint))
	}

	recvCfg := cfg
	recvCfg.SRT.Mode = string(srt.ModeListener)

	// The receiver owns the output sink, and the sink is only flushed by its
	// deferred Close. Returning as soon as the context is cancelled would let
	// the process exit first and silently truncate every measurement written
	// this run -- so this waits for the receiver to finish, always.
	recvDone := make(chan error, 1)
	go func() { recvDone <- runReceive(ctx, recvCfg, verbose) }()

	// Give the listener a moment to bind before the caller starts knocking;
	// the dial loop retries anyway.
	select {
	case <-ctx.Done():
		return <-recvDone
	case <-time.After(300 * time.Millisecond):
	}

	sendCfg := cfg
	sendCfg.SRT.Mode = string(srt.ModeCaller)
	sendDone := make(chan error, 1)
	go func() { sendDone <- runSend(ctx, sendCfg) }()

	var firstErr error
	select {
	case <-ctx.Done():
	case err := <-sendDone:
		firstErr = err
	case err := <-recvDone:
		// The receiver already returned, so its sink is flushed.
		return err
	}

	// Wait for the receiver to drain and flush, but never hang on it.
	select {
	case err := <-recvDone:
		if firstErr == nil {
			firstErr = err
		}
	case <-time.After(3 * time.Second):
	}
	return firstErr
}

// runRoundTrip publishes to a server and reads the same stream back, scoring
// the whole path through it.
func runRoundTrip(ctx context.Context, cfg config.Config, verbose bool) error {
	fmt.Fprintf(os.Stderr,
		"srtbench: round-tripping %s\n  publish as %q\n  read back as %q\n",
		cfg.SRT.Endpoint, cfg.SRT.PublishStreamID, cfg.SRT.ReadStreamID)

	// Both ends are callers here: the server listens, we do not.
	sendCfg := cfg
	sendCfg.SRT.Mode = string(srt.ModeCaller)
	recvCfg := cfg
	recvCfg.SRT.Mode = string(srt.ModeCaller)

	// The publisher starts FIRST, and the order is not arbitrary: a server has
	// no stream to hand out until something is publishing, so a reader that
	// connects first is refused. The receiver's dial retries to cover the rest
	// of the startup race.
	sendDone := make(chan error, 1)
	go func() { sendDone <- runSend(ctx, sendCfg) }()

	select {
	case <-ctx.Done():
		return nil
	case err := <-sendDone:
		if err != nil {
			return fmt.Errorf("publish leg failed: %w", err)
		}
		return nil
	case <-time.After(time.Second):
	}

	// The receiver owns the output sink and only flushes on its deferred
	// Close, so we always wait for it rather than exiting the moment the
	// sender stops.
	recvDone := make(chan error, 1)
	go func() { recvDone <- runReceive(ctx, recvCfg, verbose) }()

	var firstErr error
	select {
	case <-ctx.Done():
	case err := <-recvDone:
		return err
	case err := <-sendDone:
		if err != nil {
			firstErr = fmt.Errorf("publish leg failed: %w", err)
		}
	}
	select {
	case err := <-recvDone:
		if firstErr == nil {
			firstErr = err
		}
	case <-time.After(3 * time.Second):
	}
	return firstErr
}

// hostOf strips the scheme and query from an endpoint, for error messages.
func hostOf(endpoint string) string {
	s := strings.TrimPrefix(endpoint, "srt://")
	if i := strings.IndexByte(s, '?'); i >= 0 {
		s = s[:i]
	}
	return s
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
