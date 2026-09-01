package qoe

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Every coefficient must survive a write and a read unchanged.
//
// This is the test that would have caught the bug it was written after: a Go
// struct tag on a multi-name field applies to EVERY name in it, so
// `V1, V2 float64 \`yaml:"v1"\“ gave both fields the key v1. The encoder
// rejected the struct outright, and had it not, half the coefficients would
// have loaded back as whatever the other half wrote.
func TestProfileRoundTripsEveryCoefficient(t *testing.T) {
	for name, want := range Profiles() {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "p.yaml")
			if err := SaveProfile(path, want, "test"); err != nil {
				t.Fatalf("save: %v", err)
			}
			got, err := LoadProfileFile(path)
			if err != nil {
				t.Fatalf("load: %v", err)
			}

			v, w := got.Video, want.Video
			for _, c := range []struct {
				name      string
				got, want float64
			}{
				{"V1", v.V1, w.V1}, {"V2", v.V2, w.V2}, {"V3", v.V3, w.V3},
				{"V4", v.V4, w.V4}, {"V5", v.V5, w.V5}, {"V6", v.V6, w.V6},
				{"V7", v.V7, w.V7}, {"V8", v.V8, w.V8}, {"V9", v.V9, w.V9},
				{"V10", v.V10, w.V10}, {"V11", v.V11, w.V11}, {"V12", v.V12, w.V12},
				{"OfrMaxFps", v.OfrMaxFps, w.OfrMaxFps},
				{"BrMinKbps", v.BrMinKbps, w.BrMinKbps},
				{"BrMaxKbps", v.BrMaxKbps, w.BrMaxKbps},
				{"FrMin", v.FrMin, w.FrMin}, {"FrMax", v.FrMax, w.FrMax},
			} {
				if c.got != c.want {
					t.Errorf("video.%s = %v, want %v", c.name, c.got, c.want)
				}
			}

			a, b := got.Audio, want.Audio
			if a.A1 != b.A1 || a.A2 != b.A2 || a.Bpl != b.Bpl ||
				a.IeFloor != b.IeFloor || a.IeCeil != b.IeCeil ||
				a.LossWindowMs != b.LossWindowMs || a.RescaleTo5 != b.RescaleTo5 {
				t.Errorf("audio block changed:\n got %+v\nwant %+v", a, b)
			}
			if got.MM != want.MM {
				t.Errorf("mm block changed:\n got %+v\nwant %+v", got.MM, want.MM)
			}
			if got.VMAF != want.VMAF {
				t.Errorf("vmaf block changed:\n got %+v\nwant %+v", got.VMAF, want.VMAF)
			}
			if got.Sync != want.Sync || got.Freeze != want.Freeze || got.AudioGap != want.AudioGap {
				t.Error("sync/stall blocks changed across a round trip")
			}

			// Scoring the same sample must give the same answer, which is the
			// property all of the above exists to guarantee.
			s := cleanSample()
			if x, y := NewModel(want).Evaluate(s), NewModel(got).Evaluate(s); x.MOSOverall != y.MOSOverall {
				t.Errorf("same sample scored %.6f before the round trip and %.6f after",
					x.MOSOverall, y.MOSOverall)
			}
		})
	}
}

// Provenance must be written as names, not as the enum's integer values, or a
// reordered constant silently reinterprets every profile ever saved.
func TestProvenanceIsWrittenAsNames(t *testing.T) {
	p, _ := LoadProfile("h264-1080p")
	p.Provenance.Correction = OriginFitted
	path := filepath.Join(t.TempDir(), "p.yaml")
	if err := SaveProfile(path, p, "h"); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	body := string(b)
	for _, want := range []string{"correction: fitted", "video: estimated", "stall: placeholder"} {
		if !strings.Contains(body, want) {
			t.Errorf("profile does not contain %q:\n%s", want, body)
		}
	}
	got, err := LoadProfileFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Provenance.Correction != OriginFitted {
		t.Errorf("provenance did not survive: %v", got.Provenance.Correction)
	}
}

// A fitted file layers onto its base, so the blocks calibration cannot inform
// keep their documented defaults instead of loading as zeros -- which would
// make V4 zero, Pow return NaN, and every score collapse.
func TestFittedProfileLayersOntoBase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "thin.yaml")
	os.WriteFile(path, []byte("name: thin-fit\nbase: h264-720p\ncorrection:\n  a: 0.5\n  b: 0.9\n"), 0o644)

	got, err := LoadProfileFile(path)
	if err != nil {
		t.Fatal(err)
	}
	base, _ := LoadProfile("h264-720p")
	if got.Video.V4 != base.Video.V4 {
		t.Errorf("base video coefficients were not inherited: V4 = %v, want %v",
			got.Video.V4, base.Video.V4)
	}
	if got.MM != base.MM {
		t.Error("multimedia weights were not inherited from the base")
	}
	if got.Correction.A != 0.5 || got.Correction.B != 0.9 {
		t.Errorf("correction not applied: %+v", got.Correction)
	}
}

// A profile that would make the model produce NaN must be refused on load,
// loudly, rather than quietly scoring garbage.
func TestValidateRejectsUnusableProfiles(t *testing.T) {
	for name, mut := range map[string]func(*Profile){
		"zero V4":          func(p *Profile) { p.Video.V4 = 0 },
		"negative V5":      func(p *Profile) { p.Video.V5 = -1 },
		"zero audio A1":    func(p *Profile) { p.Audio.A1 = 0 },
		"zero loss window": func(p *Profile) { p.Audio.LossWindowMs = 0 },
		"F4 at 1":          func(p *Profile) { p.Freeze.F4 = 1 },
		"zero vmaf width":  func(p *Profile) { p.VMAF.W2 = 0 },
	} {
		p, _ := LoadProfile("h264-1080p")
		mut(&p)
		if err := p.Validate(); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	good, _ := LoadProfile("h264-1080p")
	if err := good.Validate(); err != nil {
		t.Errorf("a shipped profile failed validation: %v", err)
	}
}

func TestLoadProfileFileRejectsUnknownBase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "b.yaml")
	os.WriteFile(path, []byte("name: x\nbase: no-such-profile\n"), 0o644)
	if _, err := LoadProfileFile(path); err == nil {
		t.Fatal("a profile naming an unknown base was accepted")
	}
}
