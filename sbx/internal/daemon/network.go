//go:build linux

package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
)

// Every sandbox is a clone of a snapshot, so every guest has the same IP and MAC. Each one
// gets its own network namespace holding an identical tap device, and the namespace
// rewrites the guest's address to a unique veth address on the way out (Firecracker's
// network-for-clones pattern). The tap keeps the snapshot's MAC and a permanent neighbor
// entry: the lab measured 30 s outages with a new MAC and a 1 s ARP wait without the entry.
const (
	guestIP  = "172.16.0.2"
	tapIP    = "172.16.0.1"
	guestMAC = "06:00:ac:10:00:02"
	tapMAC   = "06:00:ac:10:00:01"
	// guestNet is the kernel ip= argument: address, gateway, netmask, device.
	guestNet = "ip=" + guestIP + "::" + tapIP + ":255.255.255.252::eth0:off"
)

func netnsName(slot int) string { return "sbx" + strconv.Itoa(slot) }
func vethName(slot int) string  { return "sbxv" + strconv.Itoa(slot) }

// vethAddrs gives slot its /31 out of 10.200.0.0/16: the host end, then the namespace end.
func vethAddrs(slot int) (string, string) {
	n := slot * 2
	return fmt.Sprintf("10.200.%d.%d", n>>8, n&255), fmt.Sprintf("10.200.%d.%d", n>>8, n&255+1)
}

// hostRules isolate sandboxes from the host and the LAN and NAT them to the internet.
// Re-applying them replaces the whole table.
const hostRules = `table inet sbx
delete table inet sbx
table inet sbx {
	set private {
		type ipv4_addr; flags interval
		elements = { 0.0.0.0/8, 10.0.0.0/8, 100.64.0.0/10, 127.0.0.0/8, 169.254.0.0/16,
			172.16.0.0/12, 192.168.0.0/16, 224.0.0.0/3 }
	}
	chain input {
		type filter hook input priority filter; policy accept;
		iifname "sbxv*" ct state established,related accept
		iifname "sbxv*" drop
	}
	chain forward {
		type filter hook forward priority filter; policy accept;
		iifname "sbxv*" ip daddr @private drop
		iifname "sbxv*" meta nfproto ipv6 drop
		oifname "sbxv*" ct state established,related accept
		oifname "sbxv*" drop
	}
	chain postrouting {
		type nat hook postrouting priority srcnat; policy accept;
		ip saddr 10.200.0.0/16 oifname != "sbxv*" masquerade
	}
}
`

const nsRules = `table ip sbx {
	chain postrouting {
		type nat hook postrouting priority srcnat; policy accept;
		oifname "eth0" masquerade
	}
}
`

func setupHostNetwork() error {
	if err := os.WriteFile("/proc/sys/net/ipv4/ip_forward", []byte("1"), 0o644); err != nil {
		return err
	}
	if err := runIn(hostRules, "nft", "-f", "-"); err != nil {
		return err
	}
	return allowIptablesForward()
}

// allowIptablesForward lets sandbox traffic through the iptables FORWARD chain. Docker and ufw
// set its policy to DROP, and a packet must pass every chain on the hook, so hostRules alone
// cannot let it through. The rules go in even when the policy is ACCEPT, because Docker may
// start later. hostRules still drops traffic to private ranges.
func allowIptablesForward() error {
	if _, err := exec.LookPath("iptables"); err != nil {
		return nil
	}
	for _, rule := range [][]string{
		{"-i", "sbxv+", "-j", "ACCEPT"},
		{"-o", "sbxv+", "-m", "conntrack", "--ctstate", "ESTABLISHED,RELATED", "-j", "ACCEPT"},
	} {
		if run("iptables", append([]string{"-C", "FORWARD"}, rule...)...) == nil {
			continue
		}
		if err := run("iptables", append([]string{"-I", "FORWARD"}, rule...)...); err != nil {
			return err
		}
	}
	return nil
}

func setupSlot(slot, uid int) error {
	ns, veth := netnsName(slot), vethName(slot)
	host, inner := vethAddrs(slot)
	teardownSlot(slot)
	if err := run("ip", "netns", "add", ns); err != nil {
		return err
	}
	root := fmt.Sprintf("link add %s type veth peer name eth0 netns %s\naddr add %s/31 dev %s\nlink set %s up\n",
		veth, ns, host, veth, veth)
	inside := fmt.Sprintf(`link set lo up
tuntap add dev tap0 mode tap user %d group %d vnet_hdr
link set tap0 address %s
addr add %s/30 dev tap0
link set tap0 up
neigh replace %s lladdr %s dev tap0 nud permanent
addr add %s/31 dev eth0
link set eth0 up
route add default via %s
`, uid, uid, tapMAC, tapIP, guestIP, guestMAC, inner, host)
	err := errors.Join(
		runIn(root, "ip", "-batch", "-"),
		runIn(inside, "ip", "-n", ns, "-batch", "-"),
		runIn(nsRules, "ip", "netns", "exec", ns, "sh", "-c", "sysctl -qw net.ipv4.ip_forward=1 && nft -f -"),
	)
	if err != nil {
		teardownSlot(slot)
	}
	return err
}

// teardownSlot deletes the namespace with its tap. The veth goes first: the kernel removes a
// deleted namespace's devices asynchronously, so the host end would otherwise linger and
// block the slot's next setup.
func teardownSlot(slot int) {
	run("ip", "link", "del", vethName(slot))
	run("ip", "netns", "del", netnsName(slot))
}

var slotNetns = regexp.MustCompile(`^sbx(\d+)$`)

// existingSlots lists slots whose namespace exists, so startup can remove leftovers.
func existingSlots() []int {
	entries, _ := os.ReadDir("/var/run/netns")
	var slots []int
	for _, e := range entries {
		if m := slotNetns.FindStringSubmatch(e.Name()); m != nil {
			n, _ := strconv.Atoi(m[1])
			slots = append(slots, n)
		}
	}
	return slots
}

// netPool hands out network slots. It keeps a few namespaces built ahead of time, so
// creating a sandbox never waits for ip and nft.
type netPool struct {
	mu      sync.Mutex
	size    int // slots 0..size-1
	want    int // ready slots to keep
	uidBase int
	used    map[int]bool // ready or assigned
	ready   []int
	kick    chan struct{}
}

var errNoSlot = errors.New("no free network slot")

func newNetPool(size, want, uidBase int) *netPool {
	return &netPool{size: size, want: want, uidBase: uidBase, used: map[int]bool{}, kick: make(chan struct{}, 1)}
}

func (p *netPool) uid(slot int) int { return p.uidBase + slot }

func (p *netPool) acquire() (int, error) {
	p.mu.Lock()
	if n := len(p.ready); n > 0 {
		slot := p.ready[n-1]
		p.ready = p.ready[:n-1]
		p.mu.Unlock()
		p.refill()
		return slot, nil
	}
	slot, ok := p.allocLocked()
	p.mu.Unlock()
	if !ok {
		return 0, errNoSlot
	}
	if err := setupSlot(slot, p.uid(slot)); err != nil {
		p.free(slot)
		return 0, err
	}
	p.refill()
	return slot, nil
}

// claim marks a slot in use by a sandbox adopted at startup.
func (p *netPool) claim(slot int) {
	p.mu.Lock()
	p.used[slot] = true
	p.mu.Unlock()
}

func (p *netPool) release(slot int) {
	teardownSlot(slot)
	p.free(slot)
	p.refill()
}

func (p *netPool) free(slot int) {
	p.mu.Lock()
	delete(p.used, slot)
	p.mu.Unlock()
}

func (p *netPool) readyCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.ready)
}

func (p *netPool) allocLocked() (int, bool) {
	for s := 0; s < p.size; s++ {
		if !p.used[s] {
			p.used[s] = true
			return s, true
		}
	}
	return 0, false
}

func (p *netPool) refill() {
	select {
	case p.kick <- struct{}{}:
	default:
	}
}

func (p *netPool) run(ctx context.Context) {
	p.refill()
	for {
		select {
		case <-ctx.Done():
			return
		case <-p.kick:
		}
		for {
			p.mu.Lock()
			if len(p.ready) >= p.want {
				p.mu.Unlock()
				break
			}
			slot, ok := p.allocLocked()
			p.mu.Unlock()
			if !ok {
				break
			}
			if err := setupSlot(slot, p.uid(slot)); err != nil {
				slog.Error("prepare network slot", "slot", slot, "err", err)
				p.free(slot)
				break
			}
			p.mu.Lock()
			p.ready = append(p.ready, slot)
			p.mu.Unlock()
		}
	}
}

func run(name string, args ...string) error {
	return runIn("", name, args...)
}

func runIn(stdin, name string, args ...string) error {
	cmd := exec.Command(name, args...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %v: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}
