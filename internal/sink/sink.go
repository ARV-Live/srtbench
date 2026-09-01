// Package sink writes measurements out: to InfluxDB 2.x, or to stdout/CSV when
// no database is configured.
//
// The offline sink is not a toy. It keeps the tool usable and testable with no
// infrastructure at all, and it is what the calibrator reads back.
package sink

import (
	"encoding/csv"
	"fmt"
	"io"
	"math"
	"os"
	"sort"
	"strconv"
	"time"

	influxdb2 "github.com/influxdata/influxdb-client-go/v2"
	"github.com/influxdata/influxdb-client-go/v2/api"
)

// Point is one measurement ready to be written.
type Point struct {
	Measurement string
	Time        time.Time
	Tags        map[string]string
	Fields      map[string]any
}

// Sink accepts points.
type Sink interface {
	Write(Point) error
	Close() error
}

// --- InfluxDB 2.x -----------------------------------------------------------

// InfluxConfig describes the target database.
type InfluxConfig struct {
	URL    string
	Token  string
	Org    string
	Bucket string
}

type influxSink struct {
	c  influxdb2.Client
	w  api.WriteAPI
	ch <-chan error
}

// NewInflux opens a non-blocking, batched writer.
//
// Non-blocking matters more than it looks: the measurement loop must never
// stall on the metrics sink, because a database hiccup would back up the SRT
// reader and produce real packet drops -- the tool would then be measuring an
// impairment it created itself.
func NewInflux(cfg InfluxConfig) (Sink, error) {
	if cfg.URL == "" {
		return nil, fmt.Errorf("influx: url is empty")
	}
	opts := influxdb2.DefaultOptions().
		SetBatchSize(1000).
		SetFlushInterval(5000).
		SetRetryBufferLimit(50000).
		SetPrecision(time.Millisecond)

	c := influxdb2.NewClientWithOptions(cfg.URL, cfg.Token, opts)
	w := c.WriteAPI(cfg.Org, cfg.Bucket)
	s := &influxSink{c: c, w: w, ch: w.Errors()}
	go func() {
		for err := range s.ch {
			fmt.Fprintf(os.Stderr, "influx write error: %v\n", err)
		}
	}()
	return s, nil
}

func (s *influxSink) Write(p Point) error {
	s.w.WritePoint(influxdb2.NewPoint(p.Measurement, p.Tags, dropNonFinite(p.Fields), p.Time))
	return nil
}

func (s *influxSink) Close() error {
	s.w.Flush()
	s.c.Close()
	return nil
}

// dropNonFinite removes NaN and Inf, which the line protocol cannot represent
// and which would poison every aggregate that touched them. A missing field is
// always better than a corrupt one -- notably MOSAudio, which is deliberately
// NaN when the stream carries no audio track.
func dropNonFinite(in map[string]any) map[string]any {
	out := make(map[string]any, len(in))
	for k, v := range in {
		if f, ok := v.(float64); ok && (math.IsNaN(f) || math.IsInf(f, 0)) {
			continue
		}
		out[k] = v
	}
	return out
}

// --- CSV / stdout -----------------------------------------------------------

type csvSink struct {
	w      *csv.Writer
	c      io.Closer
	header []string
	meas   string
}

// NewCSV writes one measurement's points to a CSV file, or to stdout when path
// is "-". Columns are fixed by the first point written.
func NewCSV(path, measurement string) (Sink, error) {
	var (
		out io.Writer = os.Stdout
		cl  io.Closer
	)
	if path != "" && path != "-" {
		f, err := os.Create(path)
		if err != nil {
			return nil, err
		}
		out, cl = f, f
	}
	return &csvSink{w: csv.NewWriter(out), c: cl, meas: measurement}, nil
}

func (s *csvSink) Write(p Point) error {
	if s.meas != "" && p.Measurement != s.meas {
		return nil
	}
	if s.header == nil {
		for k := range p.Tags {
			s.header = append(s.header, "tag_"+k)
		}
		for k := range p.Fields {
			s.header = append(s.header, k)
		}
		sort.Strings(s.header)
		s.header = append([]string{"time"}, s.header...)
		if err := s.w.Write(s.header); err != nil {
			return err
		}
	}
	rec := make([]string, len(s.header))
	rec[0] = p.Time.UTC().Format(time.RFC3339Nano)
	for i, col := range s.header[1:] {
		if v, ok := p.Tags[trimTag(col)]; ok && isTag(col) {
			rec[i+1] = v
			continue
		}
		if v, ok := p.Fields[col]; ok {
			rec[i+1] = format(v)
		}
	}
	return s.w.Write(rec)
}

func isTag(c string) bool { return len(c) > 4 && c[:4] == "tag_" }
func trimTag(c string) string {
	if isTag(c) {
		return c[4:]
	}
	return c
}

func format(v any) string {
	switch t := v.(type) {
	case float64:
		if math.IsNaN(t) || math.IsInf(t, 0) {
			return ""
		}
		return strconv.FormatFloat(t, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(t)
	case int, int64, uint64:
		return fmt.Sprint(t)
	}
	return fmt.Sprint(v)
}

func (s *csvSink) Close() error {
	s.w.Flush()
	if s.c != nil {
		return s.c.Close()
	}
	return nil
}

// --- multi ------------------------------------------------------------------

type multi []Sink

// Multi fans a point out to several sinks.
func Multi(s ...Sink) Sink { return multi(s) }

func (m multi) Write(p Point) error {
	var firstErr error
	for _, s := range m {
		if err := s.Write(p); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (m multi) Close() error {
	var firstErr error
	for _, s := range m {
		if err := s.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// Discard drops everything.
type Discard struct{}

func (Discard) Write(Point) error { return nil }
func (Discard) Close() error      { return nil }
