// Package refqual runs the full-reference quality pass: VMAF for video and
// SDR/PSNR for audio, on a duty cycle.
//
// This is the ground truth the cheap parametric model is calibrated against.
// It is expensive but affordable: measured on a 16-core box, libvmaf runs at
// 2.99x realtime at 1080p with 8 threads, versus 0.59x single-threaded for a
// bit-identical score. Threading is therefore mandatory, and subsampling is
// unnecessary.
package refqual

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// Result is one full-reference measurement.
type Result struct {
	VMAF      float64
	VMAFMin   float64
	HaveVMAF  bool
	AudioSDR  float64
	HaveAudio bool
	Frames    int
	// Aligned reports whether reference and distorted could be lined up. An
	// unaligned comparison still produces a plausible-looking number, so this
	// flag is what stops that number being trusted or fitted against.
	Aligned bool
	Model   string
}

// Spec describes how to build the reference.
type Spec struct {
	// Input is a media file used as reference. Empty means the synthetic
	// source, which is deterministic and so needs no file on this machine.
	Input      string
	Width      int
	Height     int
	FPS        int
	Threads    int
	SyntheticV string // lavfi description, e.g. testsrc2
	WithAudio  bool
	// SyntheticAudio says the sender is transmitting the 1 kHz tone rather
	// than the input file's own audio -- which is the case for the synthetic
	// source, and also for an -input file that carries no audio track. The
	// reference has to be whatever is actually being sent; comparing received
	// audio against the wrong source still yields a number, which is precisely
	// what makes it dangerous.
	SyntheticAudio bool
}

// firstKeyframePTS reports the timestamp of the first KEYFRAME in a segment.
//
// Two things here are load-bearing and were both established empirically.
//
// First, it must be the keyframe, not the first PES. A segment cut from the
// middle of a stream begins mid-GOP, and a decoder skips everything until the
// first IDR -- so the first frame that actually comes OUT is the keyframe, and
// aligning the reference to the first PES instead lands the comparison a whole
// GOP early. Measured cost of getting this wrong: VMAF 10 instead of 98.
//
// Second, it reads PACKETS, not frames. "-show_entries frame=..." returns an
// empty list on a mid-stream segment even though ffmpeg decodes it happily,
// because frame listing needs a decodable start. Packet headers carry the
// timestamp and the keyframe flag without decoding anything.
func firstKeyframePTS(ctx context.Context, path string) (float64, error) {
	out, err := exec.CommandContext(ctx, "ffprobe",
		"-v", "quiet",
		"-select_streams", "v:0",
		"-show_entries", "packet=pts_time,flags",
		"-of", "json", path).Output()
	if err != nil {
		return 0, err
	}
	var doc struct {
		Packets []struct {
			PTSTime string `json:"pts_time"`
			Flags   string `json:"flags"`
		} `json:"packets"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		return 0, err
	}
	for _, p := range doc.Packets {
		if strings.HasPrefix(p.Flags, "K") {
			return strconv.ParseFloat(p.PTSTime, 64)
		}
	}
	return 0, fmt.Errorf("no keyframe in segment (GOP longer than the capture window?)")
}

var (
	reVMAF = regexp.MustCompile(`VMAF score:\s*([0-9.]+)`)
	// asdr reports one line per channel, "SDR ch0: -3.90 dB". Older builds
	// printed a single unlabelled figure, and matching only that form is why
	// audio SDR never appeared in the output at all: the filter ran, produced
	// its numbers, and every one of them was discarded.
	reAudioSDR = regexp.MustCompile(`SDR(?:\s+ch\d+)?:?\s+(-?[0-9.]+)`)
)

// Run compares a recorded segment against a freshly built reference.
//
// baseOffset is the stream's first-ever video PTS. MPEG-TS muxers start the
// timeline at an arbitrary offset (ffmpeg uses 1.4 s by default), so the
// reference position is (segment PTS - baseOffset), not the raw PTS.
func Run(ctx context.Context, segment string, spec Spec, baseOffset float64) (Result, error) {
	var r Result
	r.Model = "vmaf_v0.6.1"

	pts, err := firstKeyframePTS(ctx, segment)
	if err != nil {
		return r, fmt.Errorf("probe segment: %w", err)
	}
	// The reference position is (keyframe PTS - the stream's opening PTS).
	// MPEG-TS timelines start at an arbitrary offset, so the raw PTS is not a
	// source time.
	start := pts - baseOffset
	if start < 0 {
		start = 0
	}

	threads := spec.Threads
	if threads <= 0 {
		threads = 8
	}
	src := spec.SyntheticV
	if src == "" {
		src = "testsrc2"
	}

	// Reference is input 0, seeked with -ss BEFORE the input so the seek is
	// applied at the source rather than by decoding and discarding. Distorted
	// is input 1.
	args := []string{"-hide_banner", "-loglevel", "info", "-nostdin"}
	args = append(args, "-ss", fmt.Sprintf("%.4f", start))
	if spec.Input != "" {
		args = append(args, "-i", spec.Input)
	} else {
		// The synthetic source is a pure function of time, so the reference is
		// regenerated at the right point in the timeline rather than stored --
		// which is what lets sender and receiver run on different machines
		// with no file transfer.
		args = append(args, "-f", "lavfi", "-i",
			fmt.Sprintf("%s=size=%dx%d:rate=%d", src, spec.Width, spec.Height, spec.FPS))
	}
	args = append(args, "-i", segment)

	// Both sides are normalised to the same geometry, rate and pixel format:
	// libvmaf refuses mismatched inputs, and a silent rescale would change
	// what is being measured. Each is rebased to zero so frame N meets frame N.
	filter := fmt.Sprintf(
		"[1:v]scale=%d:%d,fps=%d,format=yuv420p,setpts=PTS-STARTPTS[dist];"+
			"[0:v]scale=%d:%d,fps=%d,format=yuv420p,setpts=PTS-STARTPTS[ref];"+
			"[dist][ref]libvmaf=n_threads=%d:shortest=1",
		spec.Width, spec.Height, spec.FPS,
		spec.Width, spec.Height, spec.FPS,
		threads)

	args = append(args, "-lavfi", filter, "-f", "null", "-")

	cmd := exec.CommandContext(ctx, "ffmpeg", args...)
	// NOTE: ffmpeg prints the VMAF score at INFO level. Running this child at
	// -loglevel error or warning silently discards the result, which is an
	// easy and very confusing bug to introduce.
	out, err := cmd.CombinedOutput()
	text := string(out)
	if m := reVMAF.FindStringSubmatch(text); m != nil {
		if v, e := strconv.ParseFloat(m[1], 64); e == nil {
			r.VMAF, r.HaveVMAF, r.Aligned = v, true, true
		}
	}
	if !r.HaveVMAF {
		return r, fmt.Errorf("no VMAF score produced: %s", tail(text, 400))
	}
	if err != nil && !r.HaveVMAF {
		return r, err
	}
	return r, nil
}

// RunAudio measures audio signal-to-distortion against the reference.
//
// Audio SDR is a waveform difference, and every perceptual codec deliberately
// produces a waveform that differs enormously from the source while sounding
// identical -- so on a transcoded stream this mostly reflects bitrate, not
// perceived quality. It is a change detector and a validity cross-check, and
// must NOT be the thing audio coefficients are fitted against.
func RunAudio(ctx context.Context, segment string, spec Spec, baseOffset float64) (float64, bool) {
	if !spec.WithAudio {
		return 0, false
	}
	args := []string{"-hide_banner", "-loglevel", "info", "-nostdin", "-i", segment}
	filter := "[0:a][1:a]asdr"

	if spec.SyntheticAudio || spec.Input == "" {
		// A stationary tone is identical at every instant, so it needs no
		// seeking to line up with the segment.
		args = append(args, "-f", "lavfi", "-i", "sine=frequency=1000:sample_rate=48000")
	} else {
		// The file's own audio is what is on the wire, so the reference is the
		// matching stretch of that file -- located the same way the video
		// reference is, at (segment keyframe PTS - the stream's opening PTS).
		// Measuring it against the tone instead compared two unrelated signals
		// and reported total destruction for an intact stream.
		pts, err := firstKeyframePTS(ctx, segment)
		if err != nil {
			return 0, false
		}
		start := pts - baseOffset
		if start < 0 {
			start = 0
		}
		args = append(args, "-ss", fmt.Sprintf("%.4f", start), "-i", spec.Input)
		// A real file's audio can be any rate or layout, and asdr compares
		// samples: both sides have to be brought to the same form first, and
		// rebased so sample N meets sample N.
		const norm = "aresample=48000,aformat=sample_fmts=fltp:channel_layouts=stereo,asetpts=PTS-STARTPTS"
		filter = fmt.Sprintf("[0:a]%s[dist];[1:a]%s[ref];[dist][ref]asdr", norm, norm)
	}

	// asdr stops at the shorter input, which is what bounds this against a
	// reference far longer than the segment.
	args = append(args, "-lavfi", filter, "-f", "null", "-")
	out, _ := exec.CommandContext(ctx, "ffmpeg", args...).CombinedOutput()

	// Averaged across channels: one figure per stream is what the series wants,
	// and a stereo pair of a transcoded stream tracks together.
	var sum float64
	var n int
	for _, m := range reAudioSDR.FindAllStringSubmatch(string(out), -1) {
		if v, err := strconv.ParseFloat(m[1], 64); err == nil {
			sum += v
			n++
		}
	}
	if n == 0 {
		return 0, false
	}
	return sum / float64(n), true
}

// Recorder captures a bounded slice of the received stream to disk for the
// reference pass.
type Recorder struct {
	f    *os.File
	max  int64
	n    int64
	path string
}

// NewRecorder opens a segment file that will accept at most maxBytes.
func NewRecorder(dir string, maxBytes int64) (*Recorder, error) {
	f, err := os.CreateTemp(dir, "srtbench-seg-*.ts")
	if err != nil {
		return nil, err
	}
	return &Recorder{f: f, max: maxBytes, path: f.Name()}, nil
}

func (r *Recorder) Write(b []byte) (int, error) {
	if r.n >= r.max {
		return len(b), nil // full: accept and discard, never block the reader
	}
	n, err := r.f.Write(b)
	r.n += int64(n)
	return len(b), err
}

// Full reports whether the segment has reached its size limit.
func (r *Recorder) Full() bool { return r.n >= r.max }

// Path returns the file, closing it first.
func (r *Recorder) Path() string { r.f.Close(); return r.path }

// Remove deletes the segment.
func (r *Recorder) Remove() { r.f.Close(); os.Remove(r.path) }

// TempDir returns a directory for segments.
func TempDir() string { return filepath.Join(os.TempDir(), "srtbench") }

func tail(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return "..." + s[len(s)-n:]
}
