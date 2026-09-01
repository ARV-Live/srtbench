package qoe

import (
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// LoadProfileFile reads a fitted profile from disk.
//
// A fitted profile is layered over a shipped one rather than replacing it: the
// blocks a calibration run could not touch -- the multimedia weights above all,
// which no full-reference metric can inform -- keep their documented defaults
// instead of arriving as zeros that would silently produce MOS 1 everywhere.
func LoadProfileFile(path string) (Profile, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Profile{}, fmt.Errorf("read profile: %w", err)
	}

	// Read the base name first so the file only has to carry what changed.
	var head struct {
		Name string `yaml:"name"`
		Base string `yaml:"base"`
	}
	if err := yaml.Unmarshal(b, &head); err != nil {
		return Profile{}, fmt.Errorf("parse profile: %w", err)
	}
	base := head.Base
	if base == "" {
		base = DefaultProfileName
	}
	p, err := LoadProfile(base)
	if err != nil {
		return Profile{}, fmt.Errorf("profile %q has unknown base %q: %w", path, base, err)
	}

	if err := yaml.Unmarshal(b, &p); err != nil {
		return Profile{}, fmt.Errorf("parse profile: %w", err)
	}
	if err := p.Validate(); err != nil {
		return Profile{}, fmt.Errorf("profile %q: %w", path, err)
	}
	return p, nil
}

// SaveProfile writes a profile as YAML.
func SaveProfile(path string, p Profile, header string) error {
	b, err := yaml.Marshal(p)
	if err != nil {
		return err
	}
	var sb strings.Builder
	for _, line := range strings.Split(strings.TrimRight(header, "\n"), "\n") {
		sb.WriteString("# " + line + "\n")
	}
	sb.WriteString("\n")
	sb.Write(b)
	return os.WriteFile(path, []byte(sb.String()), 0o644)
}

// Validate catches a profile that would produce nonsense rather than an error.
//
// The failure this exists for is quiet: a zero or negative scale parameter
// makes math.Pow return NaN, every comparison against NaN is false, and the
// model then reports a plausible-looking score computed from garbage.
func (p Profile) Validate() error {
	v := p.Video
	switch {
	case v.V3 <= 0:
		return fmt.Errorf("video.v3 must be positive (quality ceiling), got %g", v.V3)
	case v.V4 <= 0:
		return fmt.Errorf("video.v4 must be positive (bitrate scale), got %g", v.V4)
	case v.V5 <= 0:
		return fmt.Errorf("video.v5 must be positive (bitrate exponent), got %g", v.V5)
	case v.OfrMaxFps <= 0:
		return fmt.Errorf("video.ofr_max_fps must be positive, got %g", v.OfrMaxFps)
	}
	a := p.Audio
	switch {
	case a.A1 <= 0:
		return fmt.Errorf("audio.a1 must be positive (bitrate scale), got %g", a.A1)
	case a.A2 <= 0:
		return fmt.Errorf("audio.a2 must be positive, got %g", a.A2)
	case a.Bpl <= 0:
		return fmt.Errorf("audio.bpl must be positive (loss robustness), got %g", a.Bpl)
	case a.LossWindowMs <= 0:
		return fmt.Errorf("audio.loss_window_ms must be positive, got %g", a.LossWindowMs)
	}
	for name, s := range map[string]StallCoeffs{"freeze": p.Freeze, "audio_gap": p.AudioGap} {
		if s.F1 <= 0 || s.F2 <= 0 || s.F3 <= 0 {
			return fmt.Errorf("%s f1/f2/f3 must be positive, got %g/%g/%g", name, s.F1, s.F2, s.F3)
		}
		if s.F4 < 0 || s.F4 >= 1 {
			return fmt.Errorf("%s f4 must be in [0,1), got %g", name, s.F4)
		}
		if s.KFloor <= 0 || s.KFloor > 1 {
			return fmt.Errorf("%s k_floor must be in (0,1], got %g", name, s.KFloor)
		}
	}
	if p.VMAF.W2 == 0 {
		return fmt.Errorf("vmaf.w2 must be non-zero (logistic width)")
	}
	if p.Fuse.KMax < 1 {
		return fmt.Errorf("fuse.k_max must be at least 1, got %g", p.Fuse.KMax)
	}
	// The affine correction must be usable as-is when absent, so an unset
	// (zero) B is read as identity rather than as "multiply everything by nil".
	if p.Correction.B == 0 && p.Correction.A != 0 {
		return fmt.Errorf("correction.b is zero but correction.a is %g; "+
			"that maps every score to a constant", p.Correction.A)
	}
	return nil
}
