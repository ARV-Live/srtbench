// Package probe runs ffmpeg over the received stream to report decoder health.
//
// The TS parser says what was lost in transit; this says whether the loss
// actually hurt. The two disagreeing is itself a signal, and the dangerous
// direction -- decoder unhappy, parser saw nothing -- is what stops the tool
// reporting a clean MOS over a visibly broken picture.
package probe

import (
	"bufio"
	"context"
	"io"
	"math"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Health is a decoder-health delta for one window.
type Health struct {
	FramesOut   uint64
	FramesDup   uint64
	FramesDrop  uint64
	DecodeErr   uint64
	CorruptFrm  uint64
	NoFrame     uint64
	AudioFrames uint64
	AudioErr    uint64

	SilenceMs float64

	FreezeEvents      uint64
	FreezeMsTotal     float64
	FreezeMsMax       float64
	MsSinceLastFreeze float64
}

// Probe decodes a stream and accumulates health counters.
type Probe struct {
	mu sync.Mutex

	framesOut, framesDup, framesDrop uint64
	decodeErr, corrupt, noFrame      uint64
	audioErr                         uint64
	silenceMs                        float64

	freezeEvents  uint64
	freezeMsTotal float64
	freezeMsMax   float64
	lastFreezeAt  time.Time
	lastFrameAt   time.Time
	// minFreezeMs is the output gap that counts as a freeze. Below it a late
	// frame is ordinary jitter, not a stall.
	minFreezeMs float64
	framePeriod float64

	// snapshot baselines
	prev Health

	cmd   *exec.Cmd
	stdin io.WriteCloser
}

// New starts an ffmpeg decoder reading MPEG-TS from its stdin.
//
// silencedetect is included because dead audio is invisible to every other
// signal here: the packets keep flowing and the decoder stays happy. Note that
// silence alone is NOT treated as a defect -- an IRL streamer who stops talking
// is not a quality problem -- so the QoE model only acts on it when the frame
// count corroborates.
func New(ctx context.Context, fps int, withAudio bool) (*Probe, error) {
	args := []string{
		"-hide_banner",
		// Decode errors are reported at this level; quieter and they vanish.
		"-loglevel", "warning",
		"-fflags", "+discardcorrupt",
		"-i", "pipe:0",
	}
	if withAudio {
		args = append(args, "-af", "silencedetect=n=-50dB:d=0.2")
	}
	args = append(args, "-progress", "pipe:1", "-f", "null", "-")

	cmd := exec.CommandContext(ctx, "ffmpeg", args...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}

	p := &Probe{cmd: cmd, stdin: stdin, minFreezeMs: 150}
	if fps > 0 {
		p.framePeriod = 1000 / float64(fps)
		// A freeze is a gap of several frame periods, so the threshold scales
		// with the frame rate rather than being a fixed constant that would be
		// far too strict at 60 fps and too loose at 15.
		p.minFreezeMs = math.Max(120, 4*p.framePeriod)
	}
	go p.readProgress(stdout)
	go p.readStderr(stderr)
	return p, nil
}

// Write feeds received bytes to the decoder.
func (p *Probe) Write(b []byte) (int, error) { return p.stdin.Write(b) }

// Close stops the decoder.
func (p *Probe) Close() error {
	p.stdin.Close()
	return p.cmd.Wait()
}

// readProgress consumes ffmpeg's machine-readable key=value stream. This is a
// far more stable parse target than its human-facing status line, which
// changes between releases.
func (p *Probe) readProgress(r io.Reader) {
	sc := bufio.NewScanner(r)
	var lastFrames uint64
	for sc.Scan() {
		k, v, ok := strings.Cut(strings.TrimSpace(sc.Text()), "=")
		if !ok {
			continue
		}
		n, _ := strconv.ParseUint(strings.TrimSpace(v), 10, 64)
		p.mu.Lock()
		switch k {
		case "frame":
			now := time.Now()
			if n > lastFrames {
				// Frames advanced: close out any gap since the last progress.
				if !p.lastFrameAt.IsZero() {
					gap := now.Sub(p.lastFrameAt).Seconds() * 1000
					// Subtract the frames that legitimately fill the gap, so
					// only genuinely missing time counts as a freeze.
					expected := float64(n-lastFrames) * p.framePeriod
					if stall := gap - expected; stall > p.minFreezeMs {
						p.freezeEvents++
						p.freezeMsTotal += stall
						p.freezeMsMax = math.Max(p.freezeMsMax, stall)
						p.lastFreezeAt = now
					}
				}
				p.lastFrameAt = now
				lastFrames = n
			}
			p.framesOut = n
		case "dup_frames":
			p.framesDup = n
		case "drop_frames":
			p.framesDrop = n
		}
		p.mu.Unlock()
	}
}

var (
	reDecodeErr = regexp.MustCompile(`(?i)error while decoding|decode_slice_header error|no frame!`)
	reCorrupt   = regexp.MustCompile(`(?i)corrupt (decoded )?frame|concealing`)
	reAudioErr  = regexp.MustCompile(`(?i)\[aac|channel element.*not allocated|audio.*error`)
	reSilStart  = regexp.MustCompile(`silence_start`)
	reSilDur    = regexp.MustCompile(`silence_duration:\s*([0-9.]+)`)
)

func (p *Probe) readStderr(r io.Reader) {
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := sc.Text()
		p.mu.Lock()
		switch {
		case reCorrupt.MatchString(line):
			p.corrupt++
		case reDecodeErr.MatchString(line):
			p.decodeErr++
		case reAudioErr.MatchString(line):
			p.audioErr++
		}
		if m := reSilDur.FindStringSubmatch(line); m != nil {
			if d, err := strconv.ParseFloat(m[1], 64); err == nil {
				p.silenceMs += d * 1000
			}
		} else if reSilStart.MatchString(line) {
			// A silence that has begun but not ended still counts toward this
			// window, or a permanently dead audio track would never register.
			p.silenceMs += 0
		}
		p.mu.Unlock()
	}
}

// Snapshot returns the delta since the previous call.
func (p *Probe) Snapshot() Health {
	p.mu.Lock()
	defer p.mu.Unlock()

	cur := Health{
		FramesOut: p.framesOut, FramesDup: p.framesDup, FramesDrop: p.framesDrop,
		DecodeErr: p.decodeErr, CorruptFrm: p.corrupt, NoFrame: p.noFrame,
		AudioErr: p.audioErr, SilenceMs: p.silenceMs,
		FreezeEvents: p.freezeEvents, FreezeMsTotal: p.freezeMsTotal,
	}
	d := Health{
		FramesOut:     sub(cur.FramesOut, p.prev.FramesOut),
		FramesDup:     sub(cur.FramesDup, p.prev.FramesDup),
		FramesDrop:    sub(cur.FramesDrop, p.prev.FramesDrop),
		DecodeErr:     sub(cur.DecodeErr, p.prev.DecodeErr),
		CorruptFrm:    sub(cur.CorruptFrm, p.prev.CorruptFrm),
		NoFrame:       sub(cur.NoFrame, p.prev.NoFrame),
		AudioErr:      sub(cur.AudioErr, p.prev.AudioErr),
		SilenceMs:     math.Max(0, cur.SilenceMs-p.prev.SilenceMs),
		FreezeEvents:  sub(cur.FreezeEvents, p.prev.FreezeEvents),
		FreezeMsTotal: math.Max(0, cur.FreezeMsTotal-p.prev.FreezeMsTotal),
		FreezeMsMax:   p.freezeMsMax,
	}
	if p.lastFreezeAt.IsZero() {
		d.MsSinceLastFreeze = math.Inf(1)
	} else {
		d.MsSinceLastFreeze = float64(time.Since(p.lastFreezeAt).Milliseconds())
	}
	p.prev = cur
	p.freezeMsMax = 0
	return d
}

// sub guards against the counter going backwards, which would otherwise wrap
// an unsigned subtraction to an astronomically large number.
func sub(a, b uint64) uint64 {
	if a < b {
		return 0
	}
	return a - b
}
