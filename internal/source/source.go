// Package source runs ffmpeg to produce the MPEG-TS stream that gets sent.
//
// ffmpeg is used only for what it is genuinely good at -- encoding and
// decoding. The SRT socket is owned by this tool, because ffmpeg's srt://
// protocol exposes no transport statistics.
package source

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
)

// Spec describes the stream to generate.
type Spec struct {
	// Input is a media file. Empty means synthetic sources, which are
	// deterministic and so can be regenerated as a VMAF reference on a
	// different machine with no file transfer.
	Input      string
	Width      int
	Height     int
	FPS        int
	VideoCodec string
	VideoKbps  int
	AudioCodec string
	AudioKbps  int
	NoAudio    bool
	GOP        int
	// Realtime paces output at wall-clock speed. Always true for a live test;
	// without it ffmpeg emits as fast as it can and the SRT buffer instantly
	// overflows, producing drops that measure nothing but the test rig.
	Realtime bool
	// Duration bounds the run; zero means until stopped.
	Seconds int
}

// SyntheticVideo is the deterministic video source. testsrc2 carries motion
// and detail, which matters because a static pattern makes any codec look
// perfect and would flatter the bitrate half of the model.
const SyntheticVideo = "testsrc2"

// SyntheticAudio is a 1 kHz tone: trivially checkable, and its constant level
// makes genuine dropouts obvious.
const SyntheticAudio = "sine=frequency=1000:sample_rate=48000"

// Args builds the ffmpeg command line.
func (s Spec) Args() []string {
	a := []string{"-hide_banner", "-loglevel", "error", "-nostdin"}
	if s.Realtime {
		a = append(a, "-re")
	}

	if s.Input != "" {
		a = append(a, "-i", s.Input)
	} else {
		a = append(a,
			"-f", "lavfi", "-i",
			fmt.Sprintf("%s=size=%dx%d:rate=%d", SyntheticVideo, s.Width, s.Height, s.FPS))
		if !s.NoAudio {
			a = append(a, "-f", "lavfi", "-i", SyntheticAudio)
		}
	}

	a = append(a,
		"-c:v", s.VideoCodec,
		"-b:v", strconv.Itoa(s.VideoKbps)+"k",
		"-maxrate", strconv.Itoa(s.VideoKbps)+"k",
		"-bufsize", strconv.Itoa(s.VideoKbps*2)+"k",
		"-g", strconv.Itoa(s.GOP),
		"-pix_fmt", "yuv420p",
	)
	// Zero-latency tuning keeps the encoder from buffering frames, which would
	// otherwise show up as latency the network did not cause.
	switch {
	case strings.Contains(s.VideoCodec, "x264"):
		a = append(a, "-preset", "veryfast", "-tune", "zerolatency")
	case strings.Contains(s.VideoCodec, "x265"):
		a = append(a, "-preset", "veryfast", "-x265-params", "log-level=error")
	}

	if s.NoAudio {
		a = append(a, "-an")
	} else {
		a = append(a, "-c:a", s.AudioCodec, "-b:a", strconv.Itoa(s.AudioKbps)+"k", "-ar", "48000")
	}
	if s.Seconds > 0 {
		a = append(a, "-t", strconv.Itoa(s.Seconds))
	}
	// A short muxdelay keeps PCR pacing tight, so measured PCR jitter reflects
	// the network rather than the muxer.
	a = append(a, "-muxdelay", "0.1", "-f", "mpegts", "pipe:1")
	return a
}

// Process is a running ffmpeg producing MPEG-TS on stdout.
type Process struct {
	cmd  *exec.Cmd
	Out  io.ReadCloser
	errb *strings.Builder
}

// Start launches ffmpeg.
func Start(ctx context.Context, s Spec) (*Process, error) {
	cmd := exec.CommandContext(ctx, "ffmpeg", s.Args()...)
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	var eb strings.Builder
	cmd.Stderr = &eb
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start ffmpeg: %w", err)
	}
	return &Process{cmd: cmd, Out: out, errb: &eb}, nil
}

// Wait blocks until ffmpeg exits, surfacing its stderr on failure. ffmpeg
// reports real configuration problems there and nowhere else, so swallowing it
// turns a clear error into a silent empty stream.
func (p *Process) Wait() error {
	err := p.cmd.Wait()
	if err != nil {
		if msg := strings.TrimSpace(p.errb.String()); msg != "" {
			return fmt.Errorf("ffmpeg: %s", msg)
		}
	}
	return err
}

// Stderr returns whatever ffmpeg has written so far.
func (p *Process) Stderr() string { return p.errb.String() }
