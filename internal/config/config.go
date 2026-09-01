// Package config resolves settings from flags, environment and an optional
// YAML file, in that order of precedence.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the whole tool's settings.
type Config struct {
	SRT     SRT     `yaml:"srt"`
	Media   Media   `yaml:"media"`
	QoE     QoE     `yaml:"qoe"`
	Influx  Influx  `yaml:"influx"`
	Impair  Impair  `yaml:"impair"`
	Session Session `yaml:"session"`
}

// SRT describes the endpoint.
type SRT struct {
	Endpoint string        `yaml:"endpoint"`
	Mode     string        `yaml:"mode"`
	Latency  time.Duration `yaml:"latency"`
	// StreamID applies to whichever direction is running when no
	// direction-specific id is set.
	StreamID string `yaml:"streamid"`

	// PublishStreamID and ReadStreamID let one endpoint be driven in both
	// directions at once, which is what makes `run` work against a real ingest
	// rather than only against a local loopback: the sender publishes and the
	// receiver reads the same stream back, so the score covers the whole
	// round trip through the server instead of a socket talking to itself.
	//
	// These are full stream IDs, used verbatim, not bare keys -- the prefix
	// convention belongs to the server, not to srtbench. For MediaMTX that
	// means "publish:<key>" and "read:<key>".
	PublishStreamID string `yaml:"publish_streamid"`
	ReadStreamID    string `yaml:"read_streamid"`

	Passphrase string `yaml:"passphrase"`
}

// SendStreamID is the id the sender should announce.
func (s SRT) SendStreamID() string {
	if s.PublishStreamID != "" {
		return s.PublishStreamID
	}
	return s.StreamID
}

// RecvStreamID is the id the receiver should announce or accept.
func (s SRT) RecvStreamID() string {
	if s.ReadStreamID != "" {
		return s.ReadStreamID
	}
	return s.StreamID
}

// RoundTrip reports whether both directions are configured, meaning `run`
// should drive a real server rather than wire a local listener to a local
// caller.
func (s SRT) RoundTrip() bool {
	return s.PublishStreamID != "" && s.ReadStreamID != ""
}

// Media describes what the sender generates.
type Media struct {
	// Input is a media file to stream. Empty means synthetic test patterns,
	// which the receiver can regenerate locally as a VMAF reference without
	// any file needing to exist on both machines.
	Input      string `yaml:"input"`
	Width      int    `yaml:"width"`
	Height     int    `yaml:"height"`
	FPS        int    `yaml:"fps"`
	VideoCodec string `yaml:"vcodec"`
	VideoKbps  int    `yaml:"vbitrate_kbps"`
	AudioCodec string `yaml:"acodec"`
	AudioKbps  int    `yaml:"abitrate_kbps"`
	// NoAudio streams video only, for exercising the audio-absent path.
	NoAudio bool `yaml:"no_audio"`
	GOP     int  `yaml:"gop"`
}

// QoE controls scoring.
type QoE struct {
	Interval time.Duration `yaml:"interval"`
	Profile  string        `yaml:"profile"`
	// Reference enables the duty-cycled full-reference pass.
	Reference   bool          `yaml:"reference"`
	RefWindow   time.Duration `yaml:"ref_window"`
	RefPeriod   time.Duration `yaml:"ref_period"`
	VMAFThreads int           `yaml:"vmaf_threads"`
	ProfilePath string        `yaml:"profile_path"`
}

// Influx points at an existing database. The tool never ships one.
type Influx struct {
	URL    string `yaml:"url"`
	Token  string `yaml:"token"`
	Org    string `yaml:"org"`
	Bucket string `yaml:"bucket"`
	// CSV writes measurements to a file (or "-" for stdout) as well as, or
	// instead of, InfluxDB. This is what makes the tool usable with no
	// infrastructure at all.
	CSV string `yaml:"csv"`
}

// Impair deliberately damages the send path, so degradation curves can be
// measured rather than waited for.
type Impair struct {
	LossPct  float64       `yaml:"loss_pct"`
	BurstLen int           `yaml:"burst_len"`
	Delay    time.Duration `yaml:"delay"`
	JitterMs time.Duration `yaml:"jitter"`
	Seed     int64         `yaml:"seed"`
}

// Session labels a run.
type Session struct {
	ID   string `yaml:"id"`
	Host string `yaml:"host"`
	// Cell names the sweep cell a window belongs to. Calibration holds data out
	// by cell, never by window, so this tag is what makes an honest fit
	// possible at all.
	Cell string `yaml:"-"`
}

// Default returns the shipped configuration.
//
// H.264 is the default codec because libx265 is far heavier to encode, and a
// sender that cannot keep up starves the stream and produces degradation that
// looks exactly like network damage -- the measurement would then be of the
// test rig, not the link.
func Default() Config {
	host, _ := os.Hostname()
	return Config{
		SRT: SRT{
			Endpoint: "srt://127.0.0.1:8890",
			Mode:     "listener",
			Latency:  3 * time.Second,
		},
		Media: Media{
			Width: 1920, Height: 1080, FPS: 30,
			VideoCodec: "libx264", VideoKbps: 4500,
			AudioCodec: "aac", AudioKbps: 128,
			GOP: 60,
		},
		QoE: QoE{
			Interval: time.Second,
			Profile:  "h264-1080p",
			// The duty cycle exists to leave headroom on weaker hardware, not
			// because VMAF cannot keep up: measured here, libvmaf runs at
			// 2.99x realtime at 1080p with 8 threads.
			Reference: true,
			RefWindow: 5 * time.Second,
			RefPeriod: 30 * time.Second,
			// Measured: 8 threads gives a 5x speedup over single-threaded for
			// a bit-identical score, and 16 gives nothing further.
			VMAFThreads: 8,
		},
		Influx:  Influx{Bucket: "srtbench"},
		Session: Session{Host: host},
	}
}

// Load reads an optional YAML file, then applies SRTBENCH_* environment
// overrides. Flags are applied by the caller afterwards, so they win.
func Load(path string) (Config, error) {
	c := Default()
	if path != "" {
		b, err := os.ReadFile(path)
		if err != nil {
			return c, fmt.Errorf("read config: %w", err)
		}
		if err := yaml.Unmarshal(b, &c); err != nil {
			return c, fmt.Errorf("parse config: %w", err)
		}
	}
	c.applyEnv()
	return c, nil
}

func (c *Config) applyEnv() {
	env := func(k string, set func(string)) {
		if v, ok := os.LookupEnv("SRTBENCH_" + k); ok && v != "" {
			set(v)
		}
	}
	env("ENDPOINT", func(v string) { c.SRT.Endpoint = v })
	env("MODE", func(v string) { c.SRT.Mode = v })
	env("STREAMID", func(v string) { c.SRT.StreamID = v })
	env("PUBLISH_STREAMID", func(v string) { c.SRT.PublishStreamID = v })
	env("READ_STREAMID", func(v string) { c.SRT.ReadStreamID = v })
	env("PASSPHRASE", func(v string) { c.SRT.Passphrase = v })
	env("LATENCY", func(v string) {
		if d, err := time.ParseDuration(v); err == nil {
			c.SRT.Latency = d
		}
	})
	env("PROFILE", func(v string) { c.QoE.Profile = v })
	env("INFLUX_URL", func(v string) { c.Influx.URL = v })
	env("INFLUX_TOKEN", func(v string) { c.Influx.Token = v })
	env("INFLUX_ORG", func(v string) { c.Influx.Org = v })
	env("INFLUX_BUCKET", func(v string) { c.Influx.Bucket = v })
	env("CSV", func(v string) { c.Influx.CSV = v })
	env("SESSION_ID", func(v string) { c.Session.ID = v })
	env("VBITRATE_KBPS", func(v string) {
		if n, err := strconv.Atoi(v); err == nil {
			c.Media.VideoKbps = n
		}
	})
	env("ABITRATE_KBPS", func(v string) {
		if n, err := strconv.Atoi(v); err == nil {
			c.Media.AudioKbps = n
		}
	})
	env("FPS", func(v string) {
		if n, err := strconv.Atoi(v); err == nil {
			c.Media.FPS = n
		}
	})
	env("VCODEC", func(v string) { c.Media.VideoCodec = v })
	env("INPUT", func(v string) { c.Media.Input = v })
	env("PROFILE_FILE", func(v string) { c.QoE.ProfilePath = v })
	// Resolution as WxH, matching the -size flag. Without this a containerised
	// run could set a 720p PROFILE while still streaming 1080p, and the score
	// would be computed against the wrong coefficients with nothing to show for
	// it -- the most quietly wrong state the tool can be in.
	env("SIZE", func(v string) {
		var w, h int
		if _, err := fmt.Sscanf(v, "%dx%d", &w, &h); err == nil && w > 0 && h > 0 {
			c.Media.Width, c.Media.Height = w, h
		}
	})
}

// Validate reports configuration that cannot work, before anything is started.
func (c Config) Validate() error {
	if c.SRT.Endpoint == "" {
		return fmt.Errorf("srt endpoint is empty")
	}
	if !strings.HasPrefix(c.SRT.Endpoint, "srt://") {
		return fmt.Errorf("srt endpoint %q must begin with srt://", c.SRT.Endpoint)
	}
	if c.QoE.Interval <= 0 {
		return fmt.Errorf("qoe interval must be positive")
	}
	if c.Influx.URL == "" && c.Influx.CSV == "" {
		return fmt.Errorf("no output configured: set influx.url or influx.csv " +
			"(use csv: \"-\" to print to stdout)")
	}
	if c.Influx.URL != "" && c.Influx.Bucket == "" {
		return fmt.Errorf("influx.url is set but influx.bucket is empty")
	}
	if c.Impair.LossPct < 0 || c.Impair.LossPct > 100 {
		return fmt.Errorf("impair.loss_pct %.2f is outside 0..100", c.Impair.LossPct)
	}
	if err := c.checkProfileMatchesMedia(); err != nil {
		return err
	}
	return nil
}

// checkProfileMatchesMedia catches a profile that does not describe the media
// being sent.
//
// The coefficients are codec- and resolution-specific, so scoring 1080p HEVC
// with the h264-720p profile produces a confident number computed against the
// wrong curve. Nothing else in the system would notice, which is exactly why it
// is worth refusing up front.
func (c Config) checkProfileMatchesMedia() error {
	// A fitted profile names its own base and may legitimately be called
	// anything, so this only applies to the shipped names.
	if c.QoE.ProfilePath != "" || c.QoE.Profile == "" {
		return nil
	}
	name := c.QoE.Profile
	wantCodec := "h264"
	if strings.Contains(c.Media.VideoCodec, "265") || strings.Contains(c.Media.VideoCodec, "hevc") {
		wantCodec = "h265"
	}
	if !strings.HasPrefix(name, wantCodec) {
		return fmt.Errorf(
			"profile %q does not match vcodec %q: use a %s-* profile, or change the codec.\n"+
				"The coefficients are codec-specific, so the score would be computed "+
				"against the wrong curve", name, c.Media.VideoCodec, wantCodec)
	}
	// Height is the honest discriminator; width varies with aspect ratio.
	wantRes := "1080p"
	if c.Media.Height > 0 && c.Media.Height <= 800 {
		wantRes = "720p"
	}
	if !strings.Contains(name, wantRes) {
		return fmt.Errorf(
			"profile %q does not match %dx%d: use a *-%s profile, or change the resolution.\n"+
				"The coefficients are resolution-specific, so the score would be "+
				"computed against the wrong curve", name, c.Media.Width, c.Media.Height, wantRes)
	}
	return nil
}
