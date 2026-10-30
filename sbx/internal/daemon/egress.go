//go:build linux

package daemon

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"syscall"
	"time"

	"golang.org/x/net/dns/dnsmessage"

	"github.com/kjj6198/agent-sandbox-eval/sbx/internal/proto"
)

// A sandbox with an egress allowlist is enforced outside the guest, so nothing running in it
// can opt out. Its namespace redirects DNS, HTTP, and HTTPS to sbxd and drops every other
// packet. sbxd answers DNS only for allowed names, and relays a connection only after reading
// its host name from the HTTP Host header or the TLS SNI. TLS stays end to end.

// privateRanges are never reachable from a sandbox, with or without an allowlist.
var privateRanges = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("169.254.0.0/16"),
	netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.168.0.0/16"),
	netip.MustParsePrefix("224.0.0.0/3"),
}

// egressRules go into a restricted sandbox's namespace. HOST:PORT is sbxd's egress listener,
// reached through the slot's veth. The guest's resolver is 1.1.1.1, so its DNS is redirected
// rather than answered by a resolver it was told about.
const egressRules = `table ip egress {
	chain prerouting {
		type nat hook prerouting priority dstnat; policy accept;
		iifname "tap0" tcp dport { 80, 443 } dnat to %[1]s:%[2]d
		iifname "tap0" udp dport 53 dnat to %[1]s:%[2]d
	}
	chain forward {
		type filter hook forward priority filter; policy accept;
		iifname "tap0" ct status dnat accept
		iifname "tap0" drop
	}
}
`

const (
	dnsUpstream = "1.1.1.1:53"
	maxBlocked  = 20
)

func restrictSlot(slot, port int) error {
	host, _ := vethAddrs(slot)
	return runIn(fmt.Sprintf(egressRules, host, port), "ip", "netns", "exec", netnsName(slot), "nft", "-f", "-")
}

func isPublic(ip netip.Addr) bool {
	ip = ip.Unmap()
	return ip.Is4() && !slices.ContainsFunc(privateRanges, func(p netip.Prefix) bool { return p.Contains(ip) })
}

// egressDialer refuses private addresses at connect time, so a public name that resolves to
// a LAN or metadata address cannot be used to reach it.
var egressDialer = net.Dialer{
	Timeout: 10 * time.Second,
	Control: func(_, address string, _ syscall.RawConn) error {
		ap, err := netip.ParseAddrPort(address)
		if err != nil {
			return err
		}
		if !isPublic(ap.Addr()) {
			return fmt.Errorf("%s is not a public address", ap.Addr())
		}
		return nil
	},
}

func (sb *sandbox) allows(host string) bool {
	sb.egressMu.Lock()
	defer sb.egressMu.Unlock()
	return sb.state.Egress != nil && sb.state.Egress.Allows(host)
}

func (sb *sandbox) block(host string) {
	sb.egressMu.Lock()
	defer sb.egressMu.Unlock()
	sb.blocked = slices.DeleteFunc(sb.blocked, func(b proto.Blocked) bool { return b.Host == host })
	sb.blocked = append(sb.blocked, proto.Blocked{Host: host, At: time.Now().UTC()})
	if len(sb.blocked) > maxBlocked {
		sb.blocked = sb.blocked[len(sb.blocked)-maxBlocked:]
	}
}

// SetEgress replaces the allowlist of a restricted sandbox.
func (d *Daemon) SetEgress(id string, e *proto.Egress) (*sandbox, error) {
	sb, err := d.get(id)
	if err != nil {
		return nil, err
	}
	sb.mu.Lock()
	defer sb.mu.Unlock()
	if sb.state.Egress == nil {
		return nil, fmt.Errorf("sandbox %s has the whole internet; create a new one with --allow to restrict it", id)
	}
	sb.egressMu.Lock()
	sb.state.Egress = e
	sb.egressMu.Unlock()
	return sb, d.save(sb)
}

// restrictedSandbox finds the restricted sandbox a connection to the egress listener came
// from. Its namespace masquerades it to the slot's veth address.
func (d *Daemon) restrictedSandbox(addr net.Addr) *sandbox {
	ap, err := netip.ParseAddrPort(addr.String())
	if err != nil {
		return nil
	}
	for _, sb := range d.list() {
		if _, inner := vethAddrs(sb.state.Slot); inner == ap.Addr().String() {
			sb.egressMu.Lock()
			restricted := sb.state.Egress != nil
			sb.egressMu.Unlock()
			if restricted {
				return sb
			}
			return nil
		}
	}
	return nil
}

func (d *Daemon) serveEgress(ctx context.Context) error {
	addr := fmt.Sprintf(":%d", d.cfg.EgressPort)
	ln, err := net.Listen("tcp4", addr)
	if err != nil {
		return err
	}
	pc, err := net.ListenPacket("udp4", addr)
	if err != nil {
		ln.Close()
		return err
	}
	go func() {
		<-ctx.Done()
		ln.Close()
		pc.Close()
	}()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go d.relay(c)
		}
	}()
	go d.serveDNS(pc)
	return nil
}

func (d *Daemon) relay(c net.Conn) {
	sb := d.restrictedSandbox(c.RemoteAddr())
	if sb == nil {
		c.Close()
		return
	}
	var seen bytes.Buffer
	c.SetReadDeadline(time.Now().Add(10 * time.Second))
	host, port, isHTTP := sniffHost(io.TeeReader(c, &seen))
	c.SetReadDeadline(time.Time{})
	if host == "" || !sb.allows(host) {
		if host == "" {
			host = "(connection without a host name)"
		}
		sb.block(host)
		if isHTTP {
			fmt.Fprintf(c, "HTTP/1.1 403 Forbidden\r\nContent-Type: text/plain\r\nConnection: close\r\n\r\n"+
				"sbx: %s is not in this sandbox's egress allowlist\n", host)
		}
		c.Close()
		return
	}
	up, err := egressDialer.Dial("tcp4", net.JoinHostPort(host, port))
	if err != nil {
		slog.Info("egress dial", "id", sb.state.ID, "host", host, "err", err)
		if isHTTP {
			fmt.Fprintf(c, "HTTP/1.1 502 Bad Gateway\r\nContent-Type: text/plain\r\nConnection: close\r\n\r\nsbx: %v\n", err)
		}
		c.Close()
		return
	}
	proto.Splice(c, io.MultiReader(&seen, c), up, up)
}

// sniffHost reads the start of a connection to find where it is going. Only ports 80 and 443
// are redirected here, so a TLS connection is for 443 and anything else for 80.
func sniffHost(r io.Reader) (host, port string, isHTTP bool) {
	br := bufio.NewReader(r)
	first, err := br.Peek(1)
	if err != nil {
		return "", "", false
	}
	if first[0] == 0x16 { // TLS handshake record
		return sniffSNI(br), "443", false
	}
	req, err := http.ReadRequest(br)
	if err != nil {
		return "", "", false
	}
	host = req.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	return host, "80", true
}

var errSniffed = errors.New("sniffed")

// sniffSNI lets crypto/tls parse the ClientHello and stops the handshake there.
func sniffSNI(r io.Reader) string {
	var name string
	tls.Server(readOnlyConn{r: r}, &tls.Config{
		GetConfigForClient: func(h *tls.ClientHelloInfo) (*tls.Config, error) {
			name = h.ServerName
			return nil, errSniffed
		},
	}).Handshake()
	return name
}

type readOnlyConn struct{ r io.Reader }

func (c readOnlyConn) Read(p []byte) (int, error)     { return c.r.Read(p) }
func (readOnlyConn) Write([]byte) (int, error)        { return 0, io.ErrClosedPipe }
func (readOnlyConn) Close() error                     { return nil }
func (readOnlyConn) LocalAddr() net.Addr              { return nil }
func (readOnlyConn) RemoteAddr() net.Addr             { return nil }
func (readOnlyConn) SetDeadline(time.Time) error      { return nil }
func (readOnlyConn) SetReadDeadline(time.Time) error  { return nil }
func (readOnlyConn) SetWriteDeadline(time.Time) error { return nil }

func (d *Daemon) serveDNS(pc net.PacketConn) {
	buf := make([]byte, 1500)
	for {
		n, addr, err := pc.ReadFrom(buf)
		if errors.Is(err, net.ErrClosed) {
			return
		}
		if err != nil {
			continue
		}
		if sb := d.restrictedSandbox(addr); sb != nil {
			go answerDNS(pc, addr, sb, bytes.Clone(buf[:n]))
		}
	}
}

func answerDNS(pc net.PacketConn, addr net.Addr, sb *sandbox, query []byte) {
	var p dnsmessage.Parser
	h, err := p.Start(query)
	if err != nil {
		return
	}
	q, err := p.Question()
	if err != nil {
		return
	}
	name := strings.TrimSuffix(q.Name.String(), ".")
	switch {
	case !sb.allows(name):
		sb.block(name)
		pc.WriteTo(dnsReply(h, q, dnsmessage.RCodeNameError), addr)
	case q.Type == dnsmessage.TypeHTTPS:
		// An HTTPS record can carry an ECH config, and with ECH the SNI sbxd reads is the
		// provider's public name instead of the host. No record means no ECH.
		pc.WriteTo(dnsReply(h, q, dnsmessage.RCodeSuccess), addr)
	default:
		if resp, err := forwardDNS(query); err == nil {
			pc.WriteTo(resp, addr)
		}
	}
}

func forwardDNS(query []byte) ([]byte, error) {
	c, err := net.Dial("udp4", dnsUpstream)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := c.Write(query); err != nil {
		return nil, err
	}
	buf := make([]byte, 4096)
	n, err := c.Read(buf)
	return buf[:n], err
}

func dnsReply(h dnsmessage.Header, q dnsmessage.Question, rcode dnsmessage.RCode) []byte {
	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{
		ID: h.ID, Response: true, RecursionDesired: h.RecursionDesired, RecursionAvailable: true, RCode: rcode,
	})
	b.StartQuestions()
	b.Question(q)
	out, _ := b.Finish()
	return out
}
