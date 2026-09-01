// Package ts implements just enough of the MPEG-TS transport layer to measure
// stream damage, independently of any decoder.
//
// Why this exists: SRT statistics tell us what the transport thinks it lost,
// but they cannot tell us what the *decoder* never received. Continuity
// counters can, exactly, and they do it per-PID -- which is the only way to
// attribute damage to video versus audio. In a typical H.264/AAC stream the
// video PID carries roughly twelve times as many packets as the audio PID, so
// a single stream-wide loss figure would systematically misattribute damage
// and make any audio score meaningless.
//
// The parser is deliberately pure: bytes in, numbers out, no I/O and no child
// processes, so the whole thing is unit-testable without a live stream.
package ts

import (
	"math"
	"sort"
)

const (
	// PacketSize is fixed by the standard. Every TS packet is exactly 188
	// bytes, which is what lets us resynchronise by scanning for sync bytes.
	PacketSize = 188
	SyncByte   = 0x47

	// NullPID carries stuffing only. It has no continuity guarantees worth
	// tracking and must be excluded from loss ratios or it dilutes them.
	NullPID = 0x1FFF
	PATPID  = 0x0000

	// PTS/DTS use a 90 kHz clock; PCR uses the full 27 MHz system clock.
	ptsClockHz = 90000.0
	pcrClockHz = 27000000.0

	// avBaselineWindows is how many windows are observed before the stream's
	// natural mux offset is considered known. Long enough to average out the
	// interleaving sawtooth, short enough to start scoring promptly.
	avBaselineWindows = 5
)

// Kind classifies an elementary stream. We only care about the distinction
// between video and audio; everything else is bookkeeping.
type Kind int

const (
	KindUnknown Kind = iota
	KindVideo
	KindAudio
	KindData
)

func (k Kind) String() string {
	switch k {
	case KindVideo:
		return "video"
	case KindAudio:
		return "audio"
	case KindData:
		return "data"
	}
	return "unknown"
}

// kindForStreamType maps PMT stream_type values to our coarse classification.
// Only the codecs plausibly seen on an SRT contribution feed are listed.
func kindForStreamType(st byte) Kind {
	switch st {
	case 0x01, 0x02, 0x10, 0x1B, 0x24, 0x25, 0x42, 0xD1, 0xEA:
		// MPEG-1/2, MPEG-4 part 2, H.264, HEVC, VVC, AVS, Dirac, VC-1
		return KindVideo
	case 0x03, 0x04, 0x0F, 0x11, 0x1C, 0x81, 0x82, 0x83, 0x84, 0x87:
		// MPEG audio, AAC (ADTS/LATM), LPCM, AC-3/DTS family, E-AC-3
		return KindAudio
	}
	return KindData
}

// PIDStats accumulates lifetime counters for one PID. Callers should read
// deltas via Snapshot rather than these totals; a live dashboard fed lifetime
// counters is useless.
type PIDStats struct {
	PID      uint16
	Kind     Kind
	Packets  uint64
	Bytes    uint64
	CCErrors uint64
	// CCLost estimates how many packets went missing, not merely how many
	// times continuity broke. A jump of 5 in the 4-bit counter means four
	// packets vanished, and treating that as a single "error" would badly
	// understate a burst loss.
	CCLost   uint64
	PESCount uint64

	lastCC   uint8
	haveCC   bool
	lastPTS  float64
	havePTS  bool
	seenThis bool // saw a PTS within the current snapshot window
	winPTS   float64
}

// Snapshot is a windowed view: every count is the delta since the previous
// Snapshot call, which is what the QoE model and InfluxDB both want.
type Snapshot struct {
	Packets     uint64
	Bytes       uint64
	NullPackets uint64
	Desyncs     uint64

	Video PIDDelta
	Audio PIDDelta

	// AVOffsetMS is the raw mean of (audio PTS - video PTS) paired at a common
	// arrival position. Positive means audio leads video.
	//
	// THIS IS NOT A SYNC ERROR AND MUST NOT BE PENALISED DIRECTLY. In MPEG-TS
	// the muxer deliberately runs video ahead of audio to give the video
	// decoder its lead time, so a perfectly in-sync stream shows a large,
	// constant offset -- a real ffmpeg-muxed capture measured here sits at
	// -242 ms with 63 ms of sawtooth (audio PES arrive every ~180 ms against
	// video every ~33 ms). Penalising that raw figure would push EVERY healthy
	// stream past the -185 ms saturation and cost it the full sync penalty.
	//
	// PTS defines presentation, so if the timestamps are right the playback is
	// in sync by construction, whatever order the packets arrived in.
	AVOffsetMS     float64
	AVDriftSamples int
	HaveAVOffset   bool

	// AVDriftMS is the offset's DEVIATION FROM ITS OWN BASELINE, which is the
	// part that actually indicates a fault: a constant offset is muxing, a
	// changing one is a diverging clock or a track that skipped.
	AVDriftMS    float64
	AVBaselineMS float64
	HaveAVDrift  bool

	// PCRJitterMS is the standard deviation of the observed interval between
	// PCR samples. It measures delivery pacing, which is what a congested or
	// bursty link disturbs first.
	PCRJitterMS   float64
	HavePCRJitter bool

	BitrateKbps float64
}

// PIDDelta is the per-stream half of a Snapshot.
type PIDDelta struct {
	Present  bool
	PID      uint16
	Packets  uint64
	Bytes    uint64
	CCErrors uint64
	CCLost   uint64
	// LossPct is CCLost as a percentage of the packets that *should* have
	// arrived on this PID (delivered + lost). This is the authoritative
	// effective-loss figure the QoE model consumes.
	LossPct float64
}

// Parser consumes a TS byte stream incrementally. It tolerates being fed
// arbitrary chunk sizes, since an SRT read boundary has no relationship to a
// 188-byte packet boundary.
type Parser struct {
	carry []byte

	pids    map[uint16]*PIDStats
	pmtPID  uint16
	havePMT bool

	packets     uint64
	nullPackets uint64
	desyncs     uint64
	bytes       uint64

	// PCR pacing. Jitter is measured against ARRIVAL time, not against the
	// previous PCR value: consecutive PCR values are perfect by construction
	// because the muxer wrote them from one clock, so differencing them
	// reports ~0 jitter on even a badly jittered link. What actually varies
	// is the offset between when a PCR says it should arrive and when it did.
	pcrOffsets  []float64
	arrivalSec  float64
	haveArrival bool

	// firstVideoPTS anchors the stream timeline. MPEG-TS muxers start at an
	// arbitrary offset (ffmpeg uses 1.4 s), so a reference has to be located
	// at (segment PTS - this), never at the raw PTS.
	firstVideoPTS float64
	haveFirstPTS  bool

	// Paired A/V offset observations, accumulated over the window.
	driftSum   float64
	driftCount int
	// Baseline is the stream's own natural mux offset, learned from the first
	// observations and then held. Drift is measured against it.
	baseSamples  []float64
	baseline     float64
	haveBaseline bool

	// snapshot baselines
	prevPackets, prevBytes, prevNull, prevDesync uint64
	prev                                         map[uint16]PIDStats
}

// New returns a parser with no PIDs known yet; PAT/PMT discovery populates
// them as the stream reveals its structure.
func New() *Parser {
	return &Parser{
		pids: make(map[uint16]*PIDStats),
		prev: make(map[uint16]PIDStats),
	}
}

// WriteAt feeds bytes that arrived at arrivalSec, a monotonic timestamp in
// seconds. Supplying arrival time is what makes PCR jitter meaningful; Write
// is the convenience form for callers that do not have one.
func (p *Parser) WriteAt(b []byte, arrivalSec float64) (int, error) {
	p.arrivalSec, p.haveArrival = arrivalSec, true
	return p.Write(b)
}

// Write feeds bytes into the parser. It is safe to call with partial packets;
// the remainder is carried into the next call.
func (p *Parser) Write(b []byte) (int, error) {
	n := len(b)
	if len(p.carry) > 0 {
		b = append(p.carry, b...)
		p.carry = nil
	}

	i := 0
	for {
		// Resynchronise if we are not on a sync byte. A single bad offset
		// would otherwise corrupt every subsequent PID reading.
		if i < len(b) && b[i] != SyncByte {
			j := i
			for j < len(b) && b[j] != SyncByte {
				j++
			}
			if j > i {
				p.desyncs++
			}
			i = j
			continue
		}
		if i+PacketSize > len(b) {
			break
		}
		p.packet(b[i : i+PacketSize])
		i += PacketSize
	}

	// Keep the tail for next time, bounded so a stream of garbage cannot
	// grow the buffer without limit.
	if rem := len(b) - i; rem > 0 {
		if rem > 4*PacketSize {
			i = len(b) - 4*PacketSize
		}
		p.carry = append([]byte(nil), b[i:]...)
	}
	return n, nil
}

func (p *Parser) packet(pkt []byte) {
	p.packets++
	p.bytes += PacketSize

	pid := (uint16(pkt[1]&0x1F) << 8) | uint16(pkt[2])
	if pid == NullPID {
		p.nullPackets++
		return
	}

	pusi := pkt[1]&0x40 != 0
	afc := (pkt[3] >> 4) & 0x03
	cc := pkt[3] & 0x0F

	st := p.pids[pid]
	if st == nil {
		st = &PIDStats{PID: pid, Kind: KindUnknown}
		p.pids[pid] = st
	}
	st.Packets++
	st.Bytes += PacketSize

	// Adaptation field: may carry a PCR, and signals discontinuity.
	payloadOff := 4
	discontinuity := false
	if afc == 2 || afc == 3 {
		afLen := int(pkt[4])
		if afc == 3 {
			payloadOff = 5 + afLen
		} else {
			payloadOff = PacketSize // adaptation only, no payload
		}
		if afLen > 0 {
			flags := pkt[5]
			discontinuity = flags&0x80 != 0
			if flags&0x10 != 0 && afLen >= 7 {
				p.pcr(pkt[6:12])
			}
		}
	}

	// Continuity counter only increments on packets that carry payload.
	// Adaptation-only packets repeat the previous value by design, so
	// counting them as errors would invent loss that never happened.
	if afc == 1 || afc == 3 {
		if st.haveCC && !discontinuity {
			want := (st.lastCC + 1) & 0x0F
			if cc != want {
				st.CCErrors++
				// Distance forward in the 4-bit ring is how many packets
				// went missing. A duplicate (cc == lastCC) is legal per the
				// standard and is not loss.
				if cc != st.lastCC {
					gap := (int(cc) - int(want) + 16) % 16
					st.CCLost += uint64(gap)
				}
			}
		}
		st.lastCC = cc
		st.haveCC = true
	}

	if payloadOff >= PacketSize {
		return
	}
	payload := pkt[payloadOff:]

	switch {
	case pid == PATPID && pusi:
		p.parsePAT(payload)
	case p.havePMT && pid == p.pmtPID && pusi:
		p.parsePMT(payload)
	case pusi:
		p.parsePES(st, payload)
	}
}

// pcr records how far the program clock reference drifts from arrival time.
//
// It deliberately does NOT difference consecutive PCR values. Those come from
// the muxer's own clock and are near-perfect by construction, so differencing
// them reports approximately zero jitter even on a badly jittered link -- a
// measurement that looks plausible and is worthless. Network-induced jitter
// only shows up as variance in (arrival - pcr).
func (p *Parser) pcr(b []byte) {
	if len(b) < 6 || !p.haveArrival {
		return
	}
	base := (uint64(b[0]) << 25) | (uint64(b[1]) << 17) | (uint64(b[2]) << 9) |
		(uint64(b[3]) << 1) | (uint64(b[4]) >> 7)
	ext := (uint64(b[4]&0x01) << 8) | uint64(b[5])
	t := (float64(base)*300 + float64(ext)) / pcrClockHz
	// The absolute offset is arbitrary (the two clocks share no epoch); only
	// its variance is meaningful, so the mean is removed in Snapshot.
	p.pcrOffsets = append(p.pcrOffsets, (p.arrivalSec-t)*1000.0)
}

func (p *Parser) parsePAT(b []byte) {
	if len(b) < 1 {
		return
	}
	// PUSI payloads for PSI start with a pointer_field.
	off := 1 + int(b[0])
	if off+8 > len(b) || b[off] != 0x00 {
		return
	}
	secLen := int(b[off+1]&0x0F)<<8 | int(b[off+2])
	end := off + 3 + secLen - 4 // trailing CRC32
	if end > len(b) {
		end = len(b)
	}
	for i := off + 8; i+4 <= end; i += 4 {
		prog := int(b[i])<<8 | int(b[i+1])
		pid := uint16(b[i+2]&0x1F)<<8 | uint16(b[i+3])
		if prog != 0 { // program 0 is the network PID, not a PMT
			p.pmtPID = pid
			p.havePMT = true
			return
		}
	}
}

func (p *Parser) parsePMT(b []byte) {
	if len(b) < 1 {
		return
	}
	off := 1 + int(b[0])
	if off+12 > len(b) || b[off] != 0x02 {
		return
	}
	secLen := int(b[off+1]&0x0F)<<8 | int(b[off+2])
	end := off + 3 + secLen - 4
	if end > len(b) {
		end = len(b)
	}
	infoLen := int(b[off+10]&0x0F)<<8 | int(b[off+11])
	i := off + 12 + infoLen
	for i+5 <= end {
		streamType := b[i]
		pid := uint16(b[i+1]&0x1F)<<8 | uint16(b[i+2])
		esInfoLen := int(b[i+3]&0x0F)<<8 | int(b[i+4])
		st := p.pids[pid]
		if st == nil {
			st = &PIDStats{PID: pid}
			p.pids[pid] = st
		}
		// The PMT is authoritative for classification. Guessing from PID
		// numbers works for ffmpeg defaults and breaks on real encoders.
		st.Kind = kindForStreamType(streamType)
		i += 5 + esInfoLen
	}
}

// parsePES extracts the presentation timestamp, which drives A/V sync drift.
func (p *Parser) parsePES(st *PIDStats, b []byte) {
	if len(b) < 14 || b[0] != 0x00 || b[1] != 0x00 || b[2] != 0x01 {
		return
	}
	st.PESCount++
	if b[7]&0x80 == 0 { // no PTS present
		return
	}
	v := b[9:14]
	ticks := (uint64(v[0]>>1&0x07) << 30) | (uint64(v[1]) << 22) |
		(uint64(v[2]>>1) << 15) | (uint64(v[3]) << 7) | uint64(v[4]>>1)
	pts := float64(ticks) / ptsClockHz
	if st.Kind == KindVideo && !p.haveFirstPTS {
		p.firstVideoPTS, p.haveFirstPTS = pts, true
	}
	st.lastPTS = pts
	st.havePTS = true
	st.winPTS = pts
	st.seenThis = true

	// Record a drift observation AT THIS ARRIVAL POSITION, pairing this PTS
	// against the other stream's most recent one.
	//
	// Comparing each PID's last-ever PTS instead would measure which stream
	// got truncated first, not sync drift: on any teardown the audio PID
	// simply stops earlier than the video PID, yielding an alarming
	// triple-digit "drift" that is pure artifact. Pairing at a common arrival
	// point is what makes the number mean what it claims to.
	var other *PIDStats
	switch st.Kind {
	case KindVideo:
		other = p.pick(KindAudio)
	case KindAudio:
		other = p.pick(KindVideo)
	default:
		return
	}
	if other == nil || !other.havePTS {
		return
	}
	audioPTS, videoPTS := pts, other.lastPTS
	if st.Kind == KindVideo {
		audioPTS, videoPTS = other.lastPTS, pts
	}
	p.driftSum += (audioPTS - videoPTS) * 1000.0
	p.driftCount++
}

// pick returns the busiest PID of a given kind. A program can legitimately
// carry several audio tracks; the dominant one is the one being watched.
func (p *Parser) pick(k Kind) *PIDStats {
	var best *PIDStats
	for _, s := range p.pids {
		if s.Kind != k {
			continue
		}
		if best == nil || s.Packets > best.Packets {
			best = s
		}
	}
	return best
}

// Snapshot returns deltas since the previous call and resets the window.
// elapsedSec is used only to derive bitrate.
func (p *Parser) Snapshot(elapsedSec float64) Snapshot {
	s := Snapshot{
		Packets:     p.packets - p.prevPackets,
		Bytes:       p.bytes - p.prevBytes,
		NullPackets: p.nullPackets - p.prevNull,
		Desyncs:     p.desyncs - p.prevDesync,
	}
	p.prevPackets, p.prevBytes = p.packets, p.bytes
	p.prevNull, p.prevDesync = p.nullPackets, p.desyncs

	if elapsedSec > 0 {
		s.BitrateKbps = float64(s.Bytes) * 8 / elapsedSec / 1000.0
	}

	v, a := p.pick(KindVideo), p.pick(KindAudio)
	s.Video = p.delta(v)
	s.Audio = p.delta(a)

	// The offset is the mean of paired observations, reported only when both
	// streams actually spoke during this window.
	if v != nil && a != nil && v.seenThis && a.seenThis && p.driftCount > 0 {
		s.AVOffsetMS = p.driftSum / float64(p.driftCount)
		s.AVDriftSamples = p.driftCount
		s.HaveAVOffset = true

		// Learn the stream's natural offset once, then measure against it.
		// The median resists the interleaving sawtooth far better than a mean.
		if !p.haveBaseline {
			p.baseSamples = append(p.baseSamples, s.AVOffsetMS)
			if len(p.baseSamples) >= avBaselineWindows {
				p.baseline = median(p.baseSamples)
				p.haveBaseline = true
				p.baseSamples = nil
			}
		}
		if p.haveBaseline {
			s.AVBaselineMS = p.baseline
			s.AVDriftMS = s.AVOffsetMS - p.baseline
			s.HaveAVDrift = true
		}
	}
	p.driftSum, p.driftCount = 0, 0
	for _, st := range p.pids {
		st.seenThis = false
	}

	// Jitter is the spread of the arrival-vs-PCR offset. The mean is removed
	// because the two clocks share no epoch, so only variation is meaningful.
	if n := len(p.pcrOffsets); n >= 2 {
		var mean float64
		for _, d := range p.pcrOffsets {
			mean += d
		}
		mean /= float64(n)
		var sum float64
		for _, d := range p.pcrOffsets {
			sum += (d - mean) * (d - mean)
		}
		s.PCRJitterMS = math.Sqrt(sum / float64(n))
		s.HavePCRJitter = true
	}
	p.pcrOffsets = p.pcrOffsets[:0]

	return s
}

func (p *Parser) delta(st *PIDStats) PIDDelta {
	if st == nil {
		return PIDDelta{}
	}
	prev := p.prev[st.PID]
	d := PIDDelta{
		Present:  true,
		PID:      st.PID,
		Packets:  st.Packets - prev.Packets,
		Bytes:    st.Bytes - prev.Bytes,
		CCErrors: st.CCErrors - prev.CCErrors,
		CCLost:   st.CCLost - prev.CCLost,
	}
	// Denominator is what should have arrived, not what did; otherwise a
	// window that lost most of its packets reports a misleadingly low rate.
	if total := d.Packets + d.CCLost; total > 0 {
		d.LossPct = float64(d.CCLost) / float64(total) * 100.0
	}
	p.prev[st.PID] = *st
	return d
}

// median returns the middle value, which resists the A/V interleaving sawtooth
// far better than a mean does.
func median(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	c := append([]float64(nil), v...)
	sort.Float64s(c)
	n := len(c)
	if n%2 == 1 {
		return c[n/2]
	}
	return (c[n/2-1] + c[n/2]) / 2
}

// FirstVideoPTS returns the stream's opening video timestamp, which anchors
// the timeline for full-reference comparison.
func (p *Parser) FirstVideoPTS() (float64, bool) { return p.firstVideoPTS, p.haveFirstPTS }

// PIDs exposes the discovered stream map, for debug output.
func (p *Parser) PIDs() []PIDStats {
	out := make([]PIDStats, 0, len(p.pids))
	for _, s := range p.pids {
		out = append(out, *s)
	}
	return out
}
