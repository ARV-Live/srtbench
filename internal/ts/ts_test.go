package ts

import (
	"math"
	"testing"
)

// pkt builds one 188-byte TS packet. cc is the continuity counter; payload is
// truncated or zero-padded to fill the packet.
func pkt(pid uint16, cc uint8, pusi bool, payload []byte) []byte {
	p := make([]byte, PacketSize)
	p[0] = SyncByte
	p[1] = byte(pid >> 8 & 0x1F)
	if pusi {
		p[1] |= 0x40
	}
	p[2] = byte(pid & 0xFF)
	p[3] = 0x10 | (cc & 0x0F) // payload only
	copy(p[4:], payload)
	return p
}

// patPacket announces a single program whose PMT lives on pmtPID.
func patPacket(cc uint8, pmtPID uint16) []byte {
	s := []byte{
		0x00,       // pointer_field
		0x00,       // table_id (PAT)
		0xB0, 0x0D, // section_syntax + length 13
		0x00, 0x01, // transport_stream_id
		0xC1, 0x00, 0x00,
		0x00, 0x01, // program_number 1
		byte(0xE0 | pmtPID>>8), byte(pmtPID & 0xFF),
		0x00, 0x00, 0x00, 0x00, // CRC
	}
	return pkt(PATPID, cc, true, s)
}

// pmtPacket declares one video and one audio elementary stream.
func pmtPacket(cc uint8, pmtPID, vPID, aPID uint16, vType, aType byte) []byte {
	body := []byte{
		0x02,       // table_id (PMT)
		0xB0, 0x17, // length 23
		0x00, 0x01,
		0xC1, 0x00, 0x00,
		byte(0xE0 | vPID>>8), byte(vPID & 0xFF), // PCR PID
		0xF0, 0x00, // program_info_length 0
		vType, byte(0xE0 | vPID>>8), byte(vPID & 0xFF), 0xF0, 0x00,
		aType, byte(0xE0 | aPID>>8), byte(aPID & 0xFF), 0xF0, 0x00,
		0x00, 0x00, 0x00, 0x00, // CRC
	}
	return pkt(pmtPID, cc, true, append([]byte{0x00}, body...))
}

// feed announces the program then returns the parser ready for payload.
func feed(t *testing.T) *Parser {
	t.Helper()
	p := New()
	p.Write(patPacket(0, 4096))
	// 0x1B = H.264, 0x0F = AAC ADTS
	p.Write(pmtPacket(0, 4096, 256, 257, 0x1B, 0x0F))
	return p
}

func TestPMTClassifiesStreams(t *testing.T) {
	p := feed(t)
	p.Write(pkt(256, 0, false, nil))
	p.Write(pkt(257, 0, false, nil))

	s := p.Snapshot(1)
	if !s.Video.Present || s.Video.PID != 256 {
		t.Fatalf("video PID not discovered from PMT: %+v", s.Video)
	}
	if !s.Audio.Present || s.Audio.PID != 257 {
		t.Fatalf("audio PID not discovered from PMT: %+v", s.Audio)
	}
}

func TestCleanStreamHasNoLoss(t *testing.T) {
	p := feed(t)
	for i := 0; i < 32; i++ {
		p.Write(pkt(256, uint8(i&0x0F), false, nil))
	}
	s := p.Snapshot(1)
	if s.Video.CCErrors != 0 || s.Video.CCLost != 0 {
		t.Fatalf("clean stream reported loss: errors=%d lost=%d", s.Video.CCErrors, s.Video.CCLost)
	}
	if s.Video.LossPct != 0 {
		t.Fatalf("clean stream reported LossPct=%v", s.Video.LossPct)
	}
}

// A gap must be counted as the NUMBER OF PACKETS lost, not as a single event.
// Treating a jump of 5 as one error would badly understate a burst.
func TestCCGapCountsPacketsNotEvents(t *testing.T) {
	p := feed(t)
	p.Write(pkt(256, 0, false, nil))
	p.Write(pkt(256, 5, false, nil)) // 1,2,3,4 missing => 4 packets lost

	s := p.Snapshot(1)
	if s.Video.CCErrors != 1 {
		t.Fatalf("want 1 discontinuity event, got %d", s.Video.CCErrors)
	}
	if s.Video.CCLost != 4 {
		t.Fatalf("want 4 packets lost, got %d", s.Video.CCLost)
	}
}

// Damage on the audio PID must not be attributed to video, and vice versa.
// This separability is the whole reason for per-PID accounting.
func TestPerPIDSeparation(t *testing.T) {
	p := feed(t)
	for i := 0; i < 16; i++ {
		p.Write(pkt(256, uint8(i&0x0F), false, nil))
	}
	p.Write(pkt(257, 0, false, nil))
	p.Write(pkt(257, 3, false, nil)) // 2 audio packets lost

	s := p.Snapshot(1)
	if s.Video.CCLost != 0 {
		t.Fatalf("video wrongly blamed for audio loss: %d", s.Video.CCLost)
	}
	if s.Audio.CCLost != 2 {
		t.Fatalf("want 2 audio packets lost, got %d", s.Audio.CCLost)
	}
}

// A duplicate packet is legal per the standard and must not be counted as loss.
func TestDuplicateIsNotLoss(t *testing.T) {
	p := feed(t)
	p.Write(pkt(256, 3, false, nil))
	p.Write(pkt(256, 3, false, nil)) // legal duplicate

	s := p.Snapshot(1)
	if s.Video.CCLost != 0 {
		t.Fatalf("duplicate counted as %d lost packets", s.Video.CCLost)
	}
}

// The parser must tolerate being fed at arbitrary chunk boundaries, because an
// SRT read boundary has no relationship to a 188-byte packet boundary.
func TestSplitAcrossWrites(t *testing.T) {
	p := feed(t)
	var buf []byte
	for i := 0; i < 8; i++ {
		buf = append(buf, pkt(256, uint8(i), false, nil)...)
	}
	// Feed in 7-byte dribbles, which never align to 188.
	for i := 0; i < len(buf); i += 7 {
		end := i + 7
		if end > len(buf) {
			end = len(buf)
		}
		p.Write(buf[i:end])
	}
	s := p.Snapshot(1)
	if s.Video.Packets != 8 {
		t.Fatalf("want 8 packets across split writes, got %d", s.Video.Packets)
	}
	if s.Video.CCLost != 0 {
		t.Fatalf("split writes invented %d lost packets", s.Video.CCLost)
	}
}

// Null packets carry stuffing only and must be excluded from loss ratios, or
// they dilute them.
func TestNullPacketsExcluded(t *testing.T) {
	p := feed(t)
	for i := 0; i < 10; i++ {
		p.Write(pkt(NullPID, uint8(i), false, nil))
	}
	s := p.Snapshot(1)
	if s.NullPackets != 10 {
		t.Fatalf("want 10 null packets, got %d", s.NullPackets)
	}
	if s.Video.Packets != 0 {
		t.Fatalf("null packets leaked into the video PID")
	}
}

// Snapshot must report deltas, not lifetime totals; lifetime counters make a
// live dashboard useless.
func TestSnapshotReturnsDeltas(t *testing.T) {
	p := feed(t)
	for i := 0; i < 4; i++ {
		p.Write(pkt(256, uint8(i), false, nil))
	}
	if got := p.Snapshot(1).Video.Packets; got != 4 {
		t.Fatalf("first window: want 4, got %d", got)
	}
	for i := 4; i < 7; i++ {
		p.Write(pkt(256, uint8(i), false, nil))
	}
	if got := p.Snapshot(1).Video.Packets; got != 3 {
		t.Fatalf("second window should report 3 new packets, got %d", got)
	}
}

// Loss percentage must be against OFFERED packets (received + lost), not
// received alone -- otherwise 50% loss is reported as 100%.
func TestLossPctUsesOfferedDenominator(t *testing.T) {
	p := feed(t)
	p.Write(pkt(256, 0, false, nil))
	p.Write(pkt(256, 2, false, nil)) // 1 lost, 2 received

	s := p.Snapshot(1)
	// 1 lost out of 3 offered = 33.3%, not 50% (1/2).
	if got := s.Video.LossPct; got < 33.0 || got > 33.7 {
		t.Fatalf("want ~33.3%% loss against offered, got %.2f%%", got)
	}
}

func TestResyncAfterGarbage(t *testing.T) {
	p := feed(t)
	p.Write([]byte{0xFF, 0xAA, 0x13}) // garbage, no sync byte
	p.Write(pkt(256, 0, false, nil))
	s := p.Snapshot(1)
	if s.Desyncs == 0 {
		t.Fatal("garbage did not register as a desync")
	}
	if s.Video.Packets != 1 {
		t.Fatalf("failed to resync: got %d video packets", s.Video.Packets)
	}
}

// pesPTS builds a PES packet carrying a presentation timestamp, in seconds.
func pesPTS(pid uint16, cc uint8, sec float64) []byte {
	t := uint64(sec * 90000)
	hdr := []byte{
		0x00, 0x00, 0x01, 0xE0, // start code + stream_id
		0x00, 0x00, // PES length
		0x80, 0x80, 0x05, // flags: PTS present, header length 5
		byte(0x21 | (t>>29)&0x0E),
		byte(t >> 22), byte(0x01 | (t>>14)&0xFE),
		byte(t >> 7), byte(0x01 | (t<<1)&0xFE),
	}
	return pkt(pid, cc, true, hdr)
}

// A constant mux offset is NOT a sync error. MPEG-TS deliberately runs video
// ahead of audio, so a healthy stream shows a large steady offset; penalising
// it would fire on every stream ever measured.
func TestConstantMuxOffsetIsNotDrift(t *testing.T) {
	p := feed(t)
	const offset = -0.240 // audio 240 ms behind video in PTS terms, but STEADY

	var vcc, acc uint8
	for w := 0; w < 8; w++ { // more than avBaselineWindows
		base := float64(w)
		for i := 0; i < 30; i++ {
			p.Write(pesPTS(256, vcc, base+float64(i)/30))
			vcc = (vcc + 1) & 0x0F
		}
		for i := 0; i < 5; i++ {
			p.Write(pesPTS(257, acc, base+float64(i)/5+offset))
			acc = (acc + 1) & 0x0F
		}
		s := p.Snapshot(1)
		if !s.HaveAVOffset {
			t.Fatalf("window %d: no offset observed", w)
		}
		if s.HaveAVDrift && math.Abs(s.AVDriftMS) > 60 {
			t.Fatalf("window %d: steady mux offset reported as %.1f ms of drift "+
				"(baseline %.1f, offset %.1f)", w, s.AVDriftMS, s.AVBaselineMS, s.AVOffsetMS)
		}
	}
}

// A genuinely diverging clock, on the other hand, must show up.
func TestGrowingSkewIsReportedAsDrift(t *testing.T) {
	p := feed(t)
	var vcc, acc uint8
	var last float64
	for w := 0; w < 12; w++ {
		base := float64(w)
		// Audio slips a further 40 ms every window.
		slip := -0.040 * float64(w)
		for i := 0; i < 30; i++ {
			p.Write(pesPTS(256, vcc, base+float64(i)/30))
			vcc = (vcc + 1) & 0x0F
		}
		for i := 0; i < 5; i++ {
			p.Write(pesPTS(257, acc, base+float64(i)/5+slip))
			acc = (acc + 1) & 0x0F
		}
		s := p.Snapshot(1)
		if s.HaveAVDrift {
			last = s.AVDriftMS
		}
	}
	if last > -150 {
		t.Fatalf("a clock slipping 40 ms per window was not caught: drift %.1f ms", last)
	}
}
