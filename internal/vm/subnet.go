package vm

import (
	"fmt"
	"net"
	"net/netip"
)

// freeGuestSubnet picks the guest network's /24 for a VZ guest: the first
// of 192.168.64.0/24 (vmnet's own default) to 192.168.254.0/24 that
// overlaps none of used. A fixed subnet that is already on the host (a Mac
// that is itself a VM on 192.168.64.0/24, such as the GitHub macOS
// runners) makes the guest's bridge take over the host's gateway and DNS.
// vmnet only accepts subnets inside 192.168.0.0/16.
func freeGuestSubnet(used []netip.Prefix) (netip.Prefix, error) {
	for third := 64; third <= 254; third++ {
		p := netip.PrefixFrom(netip.AddrFrom4([4]byte{192, 168, byte(third), 0}), 24)
		free := true
		for _, u := range used {
			if p.Overlaps(u) {
				free = false
				break
			}
		}
		if free {
			return p, nil
		}
	}
	return netip.Prefix{}, fmt.Errorf("no free /24 in 192.168.64.0-192.168.254.0 for the guest network")
}

// hostPrefixes lists the IPv4 networks of the host's interfaces.
func hostPrefixes() ([]netip.Prefix, error) {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil, err
	}
	var out []netip.Prefix
	for _, a := range addrs {
		ipn, ok := a.(*net.IPNet)
		if !ok || ipn.IP.To4() == nil {
			continue
		}
		if p, err := netip.ParsePrefix(ipn.String()); err == nil {
			out = append(out, p.Masked())
		}
	}
	return out, nil
}

// guestNetSpec is the guest's static network (oar.net: address/prefix,
// gateway, DNS) on subnet: vmnet's gateway and DNS proxy are its first
// address, the guest takes the second.
func guestNetSpec(subnet netip.Prefix) string {
	gw := subnet.Masked().Addr().Next()
	return fmt.Sprintf("%s/%d,%s,%s", gw.Next(), subnet.Bits(), gw, gw)
}
