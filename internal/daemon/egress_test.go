//go:build linux

package daemon

import (
	"bytes"
	"crypto/tls"
	"io"
	"net"
	"net/netip"
	"strings"
	"testing"
)

func TestSniffHTTP(t *testing.T) {
	host, port, isHTTP := sniffHost(strings.NewReader("GET / HTTP/1.1\r\nHost: Example.com:80\r\n\r\n"))
	if host != "Example.com" || port != "80" || !isHTTP {
		t.Fatalf("got %q %q %v", host, port, isHTTP)
	}
}

func TestSniffTLS(t *testing.T) {
	client, server := net.Pipe()
	go tls.Client(client, &tls.Config{ServerName: "registry.npmjs.org"}).Handshake()
	var seen bytes.Buffer
	host, port, isHTTP := sniffHost(io.TeeReader(server, &seen))
	if host != "registry.npmjs.org" || port != "443" || isHTTP {
		t.Fatalf("got %q %q %v", host, port, isHTTP)
	}
	if seen.Len() == 0 || seen.Bytes()[0] != 0x16 {
		t.Fatal("the ClientHello was not kept for replay")
	}
}

func TestIsPublic(t *testing.T) {
	for addr, want := range map[string]bool{
		"1.1.1.1": true, "10.200.0.1": false, "100.72.105.4": false, "169.254.169.254": false,
		"192.168.1.6": false, "::ffff:8.8.8.8": true, "2606:4700::1111": false,
	} {
		if got := isPublic(netip.MustParseAddr(addr)); got != want {
			t.Errorf("isPublic(%s) = %v, want %v", addr, got, want)
		}
	}
}
