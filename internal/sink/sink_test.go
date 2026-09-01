package sink

import (
	"encoding/csv"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The CSV sink fixes its columns from the first row it sees, and the first row
// of any run is a warmup window. If a caller emitted a narrower field set
// there, the header would lock without the MOS columns and silently drop the
// primary measurement from the whole file -- a bug that leaves a plausible
// looking CSV with the one number you wanted missing.
func TestHeaderFromFirstRowCoversLaterFields(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "out.csv")
	s, err := NewCSV(path, "qoe")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	tags := map[string]string{"session_id": "abc"}

	// Warmup window: MOS present but not-a-number, as the publisher emits it.
	if err := s.Write(Point{Measurement: "qoe", Time: now, Tags: tags,
		Fields: map[string]any{"valid": false, "mos_overall": math.NaN()}}); err != nil {
		t.Fatal(err)
	}
	// Scored window.
	if err := s.Write(Point{Measurement: "qoe", Time: now.Add(time.Second), Tags: tags,
		Fields: map[string]any{"valid": true, "mos_overall": 4.31}}); err != nil {
		t.Fatal(err)
	}
	s.Close()

	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	rows, err := csv.NewReader(f).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Fatalf("want header + 2 rows, got %d rows", len(rows))
	}
	col := -1
	for i, h := range rows[0] {
		if h == "mos_overall" {
			col = i
		}
	}
	if col < 0 {
		t.Fatalf("mos_overall missing from header %v", rows[0])
	}
	if rows[1][col] != "" {
		t.Errorf("NaN should serialise as empty, got %q", rows[1][col])
	}
	if !strings.HasPrefix(rows[2][col], "4.31") {
		t.Errorf("scored row lost its MOS: %q", rows[2][col])
	}
}

// NaN and Inf cannot be represented in line protocol and would poison every
// aggregate that touched them. An absent field is always better than a corrupt
// one -- which is exactly how a no-audio stream reports mos_audio.
func TestNonFiniteFieldsAreDropped(t *testing.T) {
	in := map[string]any{
		"good":      4.2,
		"nan":       math.NaN(),
		"posinf":    math.Inf(1),
		"neginf":    math.Inf(-1),
		"boolfield": true,
	}
	out := dropNonFinite(in)
	for _, k := range []string{"nan", "posinf", "neginf"} {
		if _, ok := out[k]; ok {
			t.Errorf("%s survived into the write set", k)
		}
	}
	if out["good"] != 4.2 || out["boolfield"] != true {
		t.Errorf("finite fields were disturbed: %v", out)
	}
}

func TestCSVFiltersToItsMeasurement(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "out.csv")
	s, _ := NewCSV(path, "qoe")
	now := time.Now()
	s.Write(Point{Measurement: "srt_transport", Time: now, Fields: map[string]any{"pkt_recv": int64(5)}})
	s.Write(Point{Measurement: "qoe", Time: now, Fields: map[string]any{"mos_overall": 4.0}})
	s.Close()

	b, _ := os.ReadFile(path)
	if strings.Contains(string(b), "pkt_recv") {
		t.Errorf("a foreign measurement leaked into the qoe CSV:\n%s", b)
	}
	if !strings.Contains(string(b), "mos_overall") {
		t.Errorf("the qoe measurement is missing:\n%s", b)
	}
}
