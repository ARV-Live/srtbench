package refqual

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// fixture builds a short synthetic stream and returns the whole file, a slice
// cut from the middle of it, and the stream's opening video timestamp.
//
// The mid-stream slice is the case that matters: a live capture never starts
// at the beginning of the stream, and every alignment bug shows up there and
// nowhere else.
// requireVMAF skips when the local ffmpeg cannot compute VMAF.
//
// libvmaf is an optional build flag and most distribution packages omit it --
// Alpine 3.21, Debian bookworm and Debian trixie all ship an ffmpeg without it.
// These tests exercise a capability, so on a machine that lacks it they must
// SKIP: failing would report a broken tool when the tool is fine and the host
// simply cannot do full-reference scoring.
func requireVMAF(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not on PATH")
	}
	out, err := exec.Command("ffmpeg", "-hide_banner", "-filters").Output()
	if err != nil || !strings.Contains(string(out), "libvmaf") {
		t.Skip("this ffmpeg was built without libvmaf; full-reference scoring is unavailable")
	}
}

func fixture(t *testing.T) (full, mid string, basePTS float64) {
	t.Helper()
	requireVMAF(t)
	dir := t.TempDir()
	full = filepath.Join(dir, "full.ts")

	cmd := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc2=size=640x360:rate=30",
		"-c:v", "libx264", "-preset", "veryfast", "-tune", "zerolatency",
		"-b:v", "1200k",
		// A 2 s GOP. With x264's default 250-frame GOP a short capture can
		// contain no keyframe at all, and then nothing downstream can decode.
		"-g", "60",
		"-pix_fmt", "yuv420p", "-t", "10", "-muxdelay", "0.1",
		"-f", "mpegts", full)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("could not build fixture: %v %s", err, out)
	}

	b, err := os.ReadFile(full)
	if err != nil {
		t.Fatal(err)
	}
	// Cut on a 188-byte boundary, as the recorder does.
	off := (len(b) / 2 / 188) * 188
	mid = filepath.Join(dir, "mid.ts")
	if err := os.WriteFile(mid, b[off:], 0o644); err != nil {
		t.Fatal(err)
	}

	out, err := exec.Command("ffprobe", "-v", "quiet",
		"-select_streams", "v:0", "-show_entries", "packet=pts_time",
		"-of", "json", full).Output()
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Packets []struct {
			PTSTime string `json:"pts_time"`
		} `json:"packets"`
	}
	json.Unmarshal(out, &doc)
	if len(doc.Packets) == 0 {
		t.Fatal("fixture has no video packets")
	}
	basePTS, _ = strconv.ParseFloat(doc.Packets[0].PTSTime, 64)
	return full, mid, basePTS
}

func spec() Spec { return Spec{Width: 640, Height: 360, FPS: 30, Threads: 8} }

// A mid-stream segment compared against a correctly located reference must
// score very high. A misaligned comparison still returns a plausible-looking
// number -- it simply compares the wrong frames -- so a low score here means
// broken alignment, not poor video.
func TestMidStreamSegmentAlignsAndScoresHigh(t *testing.T) {
	_, mid, base := fixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	kf, err := firstKeyframePTS(ctx, mid)
	if err != nil {
		t.Fatalf("keyframe probe: %v", err)
	}
	t.Logf("stream base PTS %.4f, first keyframe in segment %.4f -> source time %.4f",
		base, kf, kf-base)

	r, err := Run(ctx, mid, spec(), base)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	t.Logf("VMAF = %.2f", r.VMAF)
	if r.VMAF < 90 {
		t.Errorf("aligned mid-stream segment scored VMAF %.2f; below 90 means the "+
			"alignment is wrong, not that the video is bad", r.VMAF)
	}
	if !r.Aligned {
		t.Error("Aligned not set on a successful comparison")
	}
}

// Aligning to the first PES instead of the first keyframe lands a whole GOP
// early. This is the specific bug that produced VMAF 10 instead of 98, so it
// is worth a test of its own.
func TestKeyframeNotFirstPacket(t *testing.T) {
	_, mid, _ := fixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	out, err := exec.CommandContext(ctx, "ffprobe", "-v", "quiet",
		"-select_streams", "v:0", "-show_entries", "packet=pts_time,flags",
		"-of", "json", mid).Output()
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Packets []struct {
			PTSTime string `json:"pts_time"`
			Flags   string `json:"flags"`
		} `json:"packets"`
	}
	json.Unmarshal(out, &doc)
	if len(doc.Packets) == 0 {
		t.Skip("no packets")
	}
	first, _ := strconv.ParseFloat(doc.Packets[0].PTSTime, 64)
	kf, err := firstKeyframePTS(ctx, mid)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("first packet PTS %.4f, first keyframe PTS %.4f, gap %.3fs",
		first, kf, kf-first)
	if kf < first {
		t.Fatal("keyframe cannot precede the first packet")
	}
}

// A deliberately misaligned reference must score clearly worse. Without this,
// a passing score above could be coincidence rather than evidence.
func TestMisalignmentScoresMuchWorse(t *testing.T) {
	_, mid, base := fixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	good, err := Run(ctx, mid, spec(), base)
	if err != nil {
		t.Fatalf("aligned: %v", err)
	}
	// Shift the reference a full second of source time.
	bad, err := Run(ctx, mid, spec(), base-1.0)
	if err != nil {
		t.Fatalf("misaligned: %v", err)
	}
	t.Logf("aligned %.2f vs misaligned %.2f", good.VMAF, bad.VMAF)
	if bad.VMAF >= good.VMAF-20 {
		t.Errorf("a 1s misalignment barely changed the score (%.2f vs %.2f); "+
			"alignment is not actually being applied", bad.VMAF, good.VMAF)
	}
}

// A capture shorter than one GOP contains no keyframe and cannot be aligned.
// That must be reported, never silently scored against an arbitrary point.
func TestNoKeyframeIsAnError(t *testing.T) {
	dir := t.TempDir()
	empty := filepath.Join(dir, "empty.ts")
	os.WriteFile(empty, make([]byte, 188*4), 0o644)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := firstKeyframePTS(ctx, empty); err == nil {
		t.Error("a segment with no keyframe was accepted")
	}
}

// asdr's output must actually parse.
//
// It did not: the pattern expected a single unlabelled figure, while ffmpeg
// prints one line per channel. RunAudio therefore ran the filter, threw every
// number away and reported "no audio measurement" on every stream it was ever
// given.
func TestAudioSDRParsesEveryFormAsdrPrints(t *testing.T) {
	for _, c := range []struct {
		name string
		line string
		want float64
	}{
		{"per channel", "[Parsed_asdr_0 @ 0x1] SDR ch0: -3.90367 dB", -3.90367},
		{"unlabelled", "[Parsed_asdr_0 @ 0x1] SDR -4.79206 dB", -4.79206},
		{"positive", "[Parsed_asdr_0 @ 0x1] SDR ch1: 22.5 dB", 22.5},
	} {
		t.Run(c.name, func(t *testing.T) {
			m := reAudioSDR.FindStringSubmatch(c.line)
			if m == nil {
				t.Fatalf("no match in %q", c.line)
			}
			got, err := strconv.ParseFloat(m[1], 64)
			if err != nil {
				t.Fatalf("parse %q: %v", m[1], err)
			}
			if got != c.want {
				t.Errorf("got %v, want %v", got, c.want)
			}
		})
	}

	// Both channels of a stereo pair must be found, since the reported figure
	// is their mean.
	both := "SDR ch0: -3.0 dB\nSDR ch1: -5.0 dB\n"
	if n := len(reAudioSDR.FindAllStringSubmatch(both, -1)); n != 2 {
		t.Errorf("found %d channels, want 2", n)
	}
}
