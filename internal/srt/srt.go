// Package srt wraps the pure-Go SRT stack and turns its lifetime counters into
// the per-window deltas the QoE model needs.
//
// We own the socket rather than letting ffmpeg do it, and that is the single
// decision the whole tool rests on: ffmpeg's srt:// protocol exposes no libsrt
// runtime statistics, so RTT, retransmissions and -- critically -- late-drop
// counts are simply unavailable through it. Without those a MOS score is
// guesswork.
package srt

import (
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	gosrt "github.com/datarhei/gosrt"
)

// Mode selects caller or listener behaviour.
type Mode string

const (
	ModeCaller   Mode = "caller"
	ModeListener Mode = "listener"
)

// Options configures a connection.
type Options struct {
	Addr        string // host:port
	Mode        Mode
	StreamID    string
	Passphrase  string
	Latency     time.Duration
	PayloadSize uint32
}

// ParseURL turns an srt://host:port?streamid=..&latency=.. URL into Options.
// Latency is accepted in microseconds, matching ffmpeg's convention, or with a
// unit suffix.
func ParseURL(raw string) (Options, error) {
	o := Options{Mode: ModeCaller, Latency: 3 * time.Second}
	s := strings.TrimPrefix(raw, "srt://")
	hostport, query, _ := strings.Cut(s, "?")
	if hostport == "" {
		return o, fmt.Errorf("srt url %q has no host:port", raw)
	}
	o.Addr = hostport

	for _, kv := range strings.Split(query, "&") {
		if kv == "" {
			continue
		}
		k, v, _ := strings.Cut(kv, "=")
		switch strings.ToLower(k) {
		case "streamid", "srt_streamid":
			o.StreamID = v
		case "passphrase":
			o.Passphrase = v
		case "mode":
			o.Mode = Mode(v)
		case "latency", "rcvlatency":
			if d, err := time.ParseDuration(v); err == nil {
				o.Latency = d
			} else if us, err := strconv.Atoi(v); err == nil {
				// Bare numbers are microseconds, as ffmpeg expects.
				o.Latency = time.Duration(us) * time.Microsecond
			}
		}
	}
	return o, nil
}

func (o Options) config() gosrt.Config {
	c := gosrt.DefaultConfig()
	if o.Latency > 0 {
		c.Latency = o.Latency
		c.ReceiverLatency = o.Latency
		c.PeerLatency = o.Latency
	}
	c.StreamId = o.StreamID
	c.Passphrase = o.Passphrase
	if o.PayloadSize > 0 {
		c.PayloadSize = o.PayloadSize
	}
	return c
}

// Dial opens a caller connection.
//
// The address is resolved here rather than left to net.Dialer, because the
// dialer follows the resolver's order and "localhost" answers ::1 first on
// most systems. A listener bound to 0.0.0.0 is IPv4-only (gosrt selects udp4
// from the parsed address), so a v6-first dial silently never connects --
// no error, just nothing. Preferring IPv4 keeps both ends on the same family.
func Dial(o Options) (gosrt.Conn, error) {
	addr, err := resolve(o.Addr, false)
	if err != nil {
		return nil, err
	}
	return gosrt.Dial("srt", addr, o.config())
}

// Listener accepts a single publishing connection.
type Listener struct {
	l    gosrt.Listener
	opts Options
}

// resolve turns host:port into ip:port, preferring IPv4.
//
// Both paths need this, for different reasons. gosrt parses a LISTEN address
// with netip.ParseAddr, which accepts a literal IP and nothing else, so a
// hostname fails outright with "unexpected character". DIAL would resolve on
// its own, but follows the resolver's order, and a v6-first answer against an
// IPv4 listener fails silently.
//
// IPv4 is preferred because SRT deployments are overwhelmingly v4 and gosrt
// binds a single family; a host with both records would otherwise strand the
// two ends on different stacks. An IPv6-only host still resolves to v6.
//
// An empty host is passed through: that is the "bind every interface" form.
func resolve(addr string, forListen bool) (string, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "", fmt.Errorf("address %q is not host:port: %w", addr, err)
	}
	if host == "" || net.ParseIP(host) != nil {
		return addr, nil
	}

	ips, err := net.LookupIP(host)
	if err != nil {
		return "", fmt.Errorf("cannot resolve %q: %w", host, err)
	}
	for _, ip := range ips {
		if v4 := ip.To4(); v4 != nil {
			return net.JoinHostPort(v4.String(), port), nil
		}
	}
	if len(ips) > 0 {
		return net.JoinHostPort(ips[0].String(), port), nil
	}
	return "", fmt.Errorf("%q resolved to no addresses", host)
}

// isLocal reports whether an address belongs to this machine.
func isLocal(host string) (bool, error) {
	ip := net.ParseIP(host)
	if ip == nil {
		return false, fmt.Errorf("not an IP: %q", host)
	}
	if ip.IsUnspecified() || ip.IsLoopback() {
		return true, nil
	}
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		// Can't tell, so don't block the attempt; the bind will report it.
		return true, nil
	}
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok && n.IP.Equal(ip) {
			return true, nil
		}
	}
	return false, nil
}

// Listen binds the SRT port.
func Listen(o Options) (*Listener, error) {
	resolved, err := resolve(o.Addr, true)
	if err != nil {
		return nil, err
	}

	// Binding an address this machine does not hold fails deep inside the OS
	// with "cannot assign requested address", which tells the operator nothing
	// about what they actually got wrong. Almost always they meant to reach a
	// remote ingest, which is caller mode, so say so.
	if host, _, err := net.SplitHostPort(resolved); err == nil {
		if local, err := isLocal(host); err == nil && !local {
			return nil, fmt.Errorf(
				"cannot listen on %s: that address belongs to another host.\n"+
					"To measure a stream FROM a remote endpoint, connect to it instead:\n"+
					"    srtbench receive -mode caller -endpoint 'srt://%s?streamid=read:<key>'\n"+
					"To push test media TO it:\n"+
					"    srtbench send -endpoint 'srt://%s?streamid=publish:<key>'\n"+
					"To accept an incoming stream on this machine, bind a local port:\n"+
					"    srtbench receive -endpoint 'srt://0.0.0.0:%s'",
				o.Addr, o.Addr, o.Addr, portOf(o.Addr))
		}
	}

	l, err := gosrt.Listen("srt", resolved, o.config())
	if err != nil {
		return nil, fmt.Errorf("listen %s: %w", resolved, err)
	}
	return &Listener{l: l, opts: o}, nil
}

func portOf(addr string) string {
	if _, port, err := net.SplitHostPort(addr); err == nil {
		return port
	}
	return "8890"
}

// Accept waits for a publisher. When a stream ID is configured, connections
// announcing a different one are rejected rather than silently measured -- a
// benchmark that quietly scores the wrong stream is worse than one that fails.
func (l *Listener) Accept() (gosrt.Conn, error) {
	for {
		req, err := l.l.Accept2()
		if err != nil {
			return nil, err
		}
		if l.opts.StreamID != "" && req.StreamId() != l.opts.StreamID {
			req.Reject(gosrt.REJ_PEER)
			continue
		}
		if req.IsEncrypted() {
			if err := req.SetPassphrase(l.opts.Passphrase); err != nil {
				req.Reject(gosrt.REJ_PEER)
				continue
			}
		}
		return req.Accept()
	}
}

func (l *Listener) Close() { l.l.Close() }

// Addr reports the bound address.
func (l *Listener) Addr() net.Addr { return nil }

// Delta is one window of SRT transport activity. Every count is the change
// since the previous sample.
type Delta struct {
	PktSent, PktRecv           uint64
	PktSendLoss, PktRecvLoss   uint64
	PktRetrans, PktRecvRetrans uint64
	PktSendDrop, PktRecvDrop   uint64
	PktRecvUndecrypt           uint64
	ByteRecv                   uint64

	MsRTT            float64
	MbpsRecvRate     float64
	MbpsSendRate     float64
	MbpsLinkCapacity float64
	MsRecvBuf        float64
	MsRecvTsbPdDelay float64
	PktFlightSize    int

	// Reset is true when a counter went backwards, which happens on reconnect.
	// The window must be discarded rather than scored.
	Reset bool
	// AvgPayloadBytes converts SRT packet counts into TS packet counts.
	AvgPayloadBytes float64
}

// Sampler turns lifetime statistics into deltas.
//
// Reset detection is not optional. SRT restarts its counters on reconnect, and
// an unsigned subtraction against a stale base yields roughly 1.8e19 -- which
// would pin the MOS at 1.0 permanently, for the rest of the run, with no
// obvious cause.
type Sampler struct {
	prev     gosrt.Statistics
	havePrev bool
}

// Sample reads the connection's statistics and returns the delta since the
// previous call.
func (s *Sampler) Sample(c gosrt.Conn) Delta {
	var st gosrt.Statistics
	c.Stats(&st)

	d := Delta{
		MsRTT:            st.Instantaneous.MsRTT,
		MbpsRecvRate:     st.Instantaneous.MbpsRecvRate,
		MbpsSendRate:     st.Instantaneous.MbpsSentRate,
		MbpsLinkCapacity: st.Instantaneous.MbpsLinkCapacity,
		MsRecvBuf:        float64(st.Instantaneous.MsRecvBuf),
		MsRecvTsbPdDelay: float64(st.Instantaneous.MsRecvTsbPdDelay),
		PktFlightSize:    int(st.Instantaneous.PktFlightSize),
	}

	if !s.havePrev {
		s.prev, s.havePrev = st, true
		d.Reset = true // first window has no baseline to difference against
		return d
	}

	a, p := st.Accumulated, s.prev.Accumulated
	if a.PktRecv < p.PktRecv || a.PktSent < p.PktSent ||
		a.ByteRecv < p.ByteRecv || st.MsTimeStamp < s.prev.MsTimeStamp {
		s.prev = st
		d.Reset = true
		return d
	}

	d.PktSent = a.PktSent - p.PktSent
	d.PktRecv = a.PktRecv - p.PktRecv
	d.PktSendLoss = a.PktSendLoss - p.PktSendLoss
	d.PktRecvLoss = a.PktRecvLoss - p.PktRecvLoss
	d.PktRetrans = a.PktRetrans - p.PktRetrans
	d.PktRecvRetrans = a.PktRecvRetrans - p.PktRecvRetrans
	d.PktSendDrop = a.PktSendDrop - p.PktSendDrop
	d.PktRecvDrop = a.PktRecvDrop - p.PktRecvDrop
	d.PktRecvUndecrypt = a.PktRecvUndecrypt - p.PktRecvUndecrypt
	d.ByteRecv = a.ByteRecv - p.ByteRecv

	// Measured payload size beats assuming the 1316 default, since a sender
	// may use anything up to 1456.
	if d.PktRecv > 0 {
		d.AvgPayloadBytes = float64(d.ByteRecv) / float64(d.PktRecv)
	}

	s.prev = st
	return d
}
