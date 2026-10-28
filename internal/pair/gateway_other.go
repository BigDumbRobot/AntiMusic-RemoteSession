//go:build !darwin

package pair

import "net/netip"

func readGateway() (netip.Addr, bool) { return netip.Addr{}, false }
