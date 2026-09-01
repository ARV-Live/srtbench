package srt

import (
	"net"
	"strings"
	"testing"
	"time"
)

// gosrt parses a listen address with netip.ParseAddr, which takes a literal IP
// and nothing else. A hostname reached it unresolved and failed with
// "unexpected character (at \"ingest.example.com\")", which says nothing about
// what to do next.
func TestResolveHostnameForListen(t *testing.T) {
	got, err := resolve("localhost:8890", true)
	if err != nil {
		t.Fatalf("localhost did not resolve: %v", err)
	}
	host, port, _ := net.SplitHostPort(got)
	if net.ParseIP(host) == nil {
		t.Errorf("resolve returned %q; host must be a literal IP for gosrt", got)
	}
	if port != "8890" {
		t.Errorf("port changed: %q", port)
	}
}

// On most systems "localhost" answers ::1 first. A listener bound to 0.0.0.0 is
// IPv4-only, so a v6 dial connects to nothing -- silently, with no error at
// either end. Both paths therefore prefer IPv4 when a host offers both.
func TestResolvePrefersIPv4(t *testing.T) {
	ips, err := net.LookupIP("localhost")
	if err != nil {
		t.Skip("no resolver")
	}
	var has4, has6 bool
	for _, ip := range ips {
		if ip.To4() != nil {
			has4 = true
		} else {
			has6 = true
		}
	}
	if !has4 || !has6 {
		t.Skip("localhost is not dual-stack here; nothing to prefer")
	}
	for _, forListen := range []bool{true, false} {
		got, err := resolve("localhost:8890", forListen)
		if err != nil {
			t.Fatal(err)
		}
		host, _, _ := net.SplitHostPort(got)
		if ip := net.ParseIP(host); ip == nil || ip.To4() == nil {
			t.Errorf("forListen=%v: got %q, want an IPv4 address", forListen, got)
		}
	}
}

// A literal IP and the bind-everything form must pass straight through.
func TestResolvePassesThroughLiterals(t *testing.T) {
	for _, addr := range []string{"127.0.0.1:8890", "0.0.0.0:8890", ":8890", "[::1]:8890"} {
		got, err := resolve(addr, true)
		if err != nil {
			t.Errorf("%s: %v", addr, err)
			continue
		}
		if got != addr {
			t.Errorf("%s was rewritten to %s", addr, got)
		}
	}
}

func TestResolveRejectsBadInput(t *testing.T) {
	if _, err := resolve("no-port-here", true); err == nil {
		t.Error("an address with no port was accepted")
	}
	if _, err := resolve("this-host-should-not-exist.invalid:8890", true); err == nil {
		t.Error("an unresolvable host was accepted")
	}
}

func TestIsLocal(t *testing.T) {
	for _, c := range []struct {
		host string
		want bool
	}{
		{"127.0.0.1", true},
		{"0.0.0.0", true},
		{"::1", true},
		// Documentation range (RFC 5737): never assigned to a real interface.
		{"203.0.113.7", false},
	} {
		got, err := isLocal(c.host)
		if err != nil {
			t.Errorf("%s: %v", c.host, err)
			continue
		}
		if got != c.want {
			t.Errorf("isLocal(%s) = %v, want %v", c.host, got, c.want)
		}
	}
}

// Binding an address owned by another host fails deep in the OS with "cannot
// assign requested address". Almost everyone hitting this meant to reach a
// remote ingest, so the error has to name caller mode.
func TestListenOnRemoteAddressExplainsItself(t *testing.T) {
	_, err := Listen(Options{Addr: "203.0.113.7:8890", Latency: time.Second})
	if err == nil {
		t.Fatal("listening on a foreign address succeeded")
	}
	msg := err.Error()
	for _, want := range []string{"another host", "-mode caller", "srtbench send", "0.0.0.0"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error does not mention %q:\n%s", want, msg)
		}
	}
}

func TestParseURL(t *testing.T) {
	o, err := ParseURL("srt://host.example:8890?streamid=publish:abc&latency=2000000&passphrase=s3cret")
	if err != nil {
		t.Fatal(err)
	}
	if o.Addr != "host.example:8890" {
		t.Errorf("addr = %q", o.Addr)
	}
	if o.StreamID != "publish:abc" {
		t.Errorf("streamid = %q", o.StreamID)
	}
	if o.Passphrase != "s3cret" {
		t.Errorf("passphrase = %q", o.Passphrase)
	}
	// A bare number is microseconds, matching ffmpeg's convention.
	if o.Latency != 2*time.Second {
		t.Errorf("latency = %v, want 2s", o.Latency)
	}
}

func TestParseURLAcceptsDurationSuffix(t *testing.T) {
	o, err := ParseURL("srt://1.2.3.4:8890?latency=300ms")
	if err != nil {
		t.Fatal(err)
	}
	if o.Latency != 300*time.Millisecond {
		t.Errorf("latency = %v, want 300ms", o.Latency)
	}
}
