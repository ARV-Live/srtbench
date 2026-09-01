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
