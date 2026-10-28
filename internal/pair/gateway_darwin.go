package pair

import (
	"net/netip"
	"os/exec"
	"strings"
)

// readGateway reads the IPv4 default gateway from the routing table.
func readGateway() (netip.Addr, bool) {
	out, err := exec.Command("/sbin/route", "-n", "get", "default").Output()
	if err != nil {
		return netip.Addr{}, false
	}
	for _, l := range strings.Split(string(out), "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(l), "gateway:"); ok {
			a, err := netip.ParseAddr(strings.TrimSpace(v))
			return a, err == nil && a.Is4()
		}
	}
	return netip.Addr{}, false
}
