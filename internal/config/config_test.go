package config

import (
	"os"
	"testing"
)

// Configuration that cannot work should fail before anything is started, with
// a message that says what to do about it.
func TestValidateCatchesUnworkableConfig(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*Config)
		want string
	}{
		{"no output", func(c *Config) { c.Influx.URL = ""; c.Influx.CSV = "" }, "no output configured"},
		{"bad scheme", func(c *Config) { c.SRT.Endpoint = "rtmp://x"; c.Influx.CSV = "-" }, "srt://"},
		{"no bucket", func(c *Config) { c.Influx.URL = "http://x"; c.Influx.Bucket = "" }, "bucket"},
		{"bad loss", func(c *Config) { c.Influx.CSV = "-"; c.Impair.LossPct = 150 }, "outside 0..100"},
		{"zero interval", func(c *Config) { c.Influx.CSV = "-"; c.QoE.Interval = 0 }, "interval"},
	}
	for _, c := range cases {
		cfg := Default()
		c.mut(&cfg)
		err := cfg.Validate()
		if err == nil {
			t.Errorf("%s: accepted a config that cannot work", c.name)
			continue
		}
		if !contains(err.Error(), c.want) {
			t.Errorf("%s: error %q does not mention %q", c.name, err, c.want)
		}
	}
}

// A CSV-only setup must be valid: the tool has to work with no database.
func TestCSVOnlyIsValid(t *testing.T) {
	cfg := Default()
	cfg.Influx.CSV = "-"
	if err := cfg.Validate(); err != nil {
		t.Fatalf("CSV-only config rejected: %v", err)
	}
}

func TestEnvOverridesDefaults(t *testing.T) {
	os.Setenv("SRTBENCH_ENDPOINT", "srt://example:1234")
	os.Setenv("SRTBENCH_INFLUX_BUCKET", "other")
	defer func() {
		os.Unsetenv("SRTBENCH_ENDPOINT")
		os.Unsetenv("SRTBENCH_INFLUX_BUCKET")
	}()
	c, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if c.SRT.Endpoint != "srt://example:1234" {
		t.Errorf("endpoint not taken from env: %q", c.SRT.Endpoint)
	}
	if c.Influx.Bucket != "other" {
		t.Errorf("bucket not taken from env: %q", c.Influx.Bucket)
	}
}

// The default profile must match the default codec and resolution, or every
// out-of-the-box run scores against the wrong coefficients.
func TestDefaultProfileMatchesDefaultMedia(t *testing.T) {
	c := Default()
	if c.Media.VideoCodec != "libx264" || c.Media.Height != 1080 {
		t.Fatalf("defaults changed: %s %dp", c.Media.VideoCodec, c.Media.Height)
	}
	if c.QoE.Profile != "h264-1080p" {
		t.Errorf("default profile %q does not match default media (libx264 1080p)", c.QoE.Profile)
	}
}

func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// The sender publishes and the receiver reads, so each direction takes its own
// stream id when one is configured.
func TestDirectionalStreamIDs(t *testing.T) {
	c := SRT{StreamID: "shared", PublishStreamID: "publish:k", ReadStreamID: "read:k"}
	if got := c.SendStreamID(); got != "publish:k" {
		t.Errorf("send id = %q, want the publish id", got)
	}
	if got := c.RecvStreamID(); got != "read:k" {
		t.Errorf("recv id = %q, want the read id", got)
	}
	if !c.RoundTrip() {
		t.Error("both ids set but RoundTrip() is false")
	}
}

// With no directional ids, both directions fall back to the shared one, so
// existing configs keep working unchanged.
func TestStreamIDFallback(t *testing.T) {
	c := SRT{StreamID: "shared"}
	if c.SendStreamID() != "shared" || c.RecvStreamID() != "shared" {
		t.Errorf("fallback broken: send=%q recv=%q", c.SendStreamID(), c.RecvStreamID())
	}
	if c.RoundTrip() {
		t.Error("RoundTrip() true with no directional ids")
	}
}

// One id alone cannot round-trip. RoundTrip() must stay false so `run` refuses
// rather than silently falling back to a loopback that never touches the
// server -- which would report a healthy score for an untested path.
func TestOneIDIsNotARoundTrip(t *testing.T) {
	if (SRT{PublishStreamID: "publish:k"}).RoundTrip() {
		t.Error("publish id alone counted as a round trip")
	}
	if (SRT{ReadStreamID: "read:k"}).RoundTrip() {
		t.Error("read id alone counted as a round trip")
	}
}

// A directional id must win over the shared one for its own direction only.
func TestDirectionalOverridesSharedIndependently(t *testing.T) {
	c := SRT{StreamID: "shared", ReadStreamID: "read:k"}
	if got := c.SendStreamID(); got != "shared" {
		t.Errorf("send id = %q; a read id must not affect the send direction", got)
	}
	if got := c.RecvStreamID(); got != "read:k" {
		t.Errorf("recv id = %q, want the read id", got)
	}
}

func TestStreamIDsFromEnv(t *testing.T) {
	os.Setenv("SRTBENCH_PUBLISH_STREAMID", "publish:env")
	os.Setenv("SRTBENCH_READ_STREAMID", "read:env")
	defer func() {
		os.Unsetenv("SRTBENCH_PUBLISH_STREAMID")
		os.Unsetenv("SRTBENCH_READ_STREAMID")
	}()
	c, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if c.SRT.PublishStreamID != "publish:env" || c.SRT.ReadStreamID != "read:env" {
		t.Errorf("env not applied: %+v", c.SRT)
	}
}

// The coefficients are codec- and resolution-specific, so a mismatched profile
// produces a confident number computed against the wrong curve. Nothing
// downstream would notice, which is why it is refused up front.
func TestProfileMustMatchMedia(t *testing.T) {
	cases := []struct {
		name    string
		codec   string
		height  int
		profile string
		wantErr string
	}{
		{"h264 1080p ok", "libx264", 1080, "h264-1080p", ""},
		{"h265 720p ok", "libx265", 720, "h265-720p", ""},
		{"codec mismatch", "libx265", 1080, "h264-1080p", "h265-*"},
		{"resolution mismatch", "libx264", 1080, "h264-720p", "*-1080p"},
		{"both wrong", "libx265", 720, "h264-1080p", "h265-*"},
	}
	for _, c := range cases {
		cfg := Default()
		cfg.Influx.CSV = "-"
		cfg.Media.VideoCodec = c.codec
		cfg.Media.Height = c.height
		cfg.QoE.Profile = c.profile

		err := cfg.Validate()
		if c.wantErr == "" {
			if err != nil {
				t.Errorf("%s: rejected a matching pair: %v", c.name, err)
			}
			continue
		}
		if err == nil {
			t.Errorf("%s: accepted %s at %dp with profile %s",
				c.name, c.codec, c.height, c.profile)
			continue
		}
		if !contains(err.Error(), c.wantErr) {
			t.Errorf("%s: error %q does not suggest %q", c.name, err, c.wantErr)
		}
	}
}

// A fitted profile can legitimately be named anything, so the check must not
// second-guess one.
func TestFittedProfileSkipsTheMediaCheck(t *testing.T) {
	cfg := Default()
	cfg.Influx.CSV = "-"
	cfg.QoE.ProfilePath = "/some/fitted.yaml"
	cfg.QoE.Profile = "h264-720p" // would otherwise clash with 1080p defaults
	if err := cfg.Validate(); err != nil {
		t.Errorf("a fitted profile was subjected to the name check: %v", err)
	}
}

// Resolution has to be settable from the environment, or a containerised run
// can set a 720p profile while still streaming 1080p.
func TestMediaFromEnv(t *testing.T) {
	for k, v := range map[string]string{
		"SRTBENCH_SIZE": "1280x720", "SRTBENCH_FPS": "60",
		"SRTBENCH_VCODEC": "libx265", "SRTBENCH_ABITRATE_KBPS": "96",
	} {
		os.Setenv(k, v)
		defer os.Unsetenv(k)
	}
	c, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if c.Media.Width != 1280 || c.Media.Height != 720 {
		t.Errorf("size = %dx%d, want 1280x720", c.Media.Width, c.Media.Height)
	}
	if c.Media.FPS != 60 {
		t.Errorf("fps = %d", c.Media.FPS)
	}
	if c.Media.VideoCodec != "libx265" {
		t.Errorf("vcodec = %q", c.Media.VideoCodec)
	}
	if c.Media.AudioKbps != 96 {
		t.Errorf("abitrate = %d", c.Media.AudioKbps)
	}
}

// The sender and the reference pass must agree on what audio is on the wire.
//
// They read this from one place for a reason: a reference compared against the
// wrong source still produces a plausible number, so a disagreement here would
// publish confident nonsense rather than fail.
func TestUsesSyntheticAudio(t *testing.T) {
	for _, c := range []struct {
		name  string
		media Media
		want  bool
	}{
		{"synthetic source", Media{}, true},
		{"file with audio", Media{Input: "clip.mp4", InputHasAudio: true}, false},
		{"file without audio", Media{Input: "clip.mp4"}, true},
		{"no-audio, synthetic", Media{NoAudio: true}, false},
		{"no-audio, file with audio", Media{Input: "clip.mp4", InputHasAudio: true, NoAudio: true}, false},
		{"no-audio, file without audio", Media{Input: "clip.mp4", NoAudio: true}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := c.media.UsesSyntheticAudio(); got != c.want {
				t.Errorf("got %v, want %v", got, c.want)
			}
		})
	}
}
