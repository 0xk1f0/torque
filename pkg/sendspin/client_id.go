// ABOUTME: MAC address picking for device_info.mac_address
// ABOUTME: Deterministic selection of the primary interface MAC.

package sendspin

import (
	"bytes"
	"net"
	"sort"
)

// allInterfaces is a package-level var so tests can stub it.
var allInterfaces = func() []net.Interface {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	return ifaces
}

// pickStableMAC returns the MAC of the alphabetically-first eligible interface.
// Eligible means: up, not loopback, 6-byte hardware address, not zero, not
// broadcast. Alphabetical ordering gives deterministic selection across reboots
// and OSes (eth0 < wlan0, Ethernet < Wi-Fi).
func pickStableMAC(ifaces []net.Interface) (string, bool) {
	eligible := make([]net.Interface, 0, len(ifaces))
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 {
			continue
		}
		if iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		if len(iface.HardwareAddr) != 6 {
			continue
		}
		if isZeroMAC(iface.HardwareAddr) || isBroadcastMAC(iface.HardwareAddr) {
			continue
		}
		eligible = append(eligible, iface)
	}
	if len(eligible) == 0 {
		return "", false
	}
	sort.Slice(eligible, func(i, j int) bool {
		return eligible[i].Name < eligible[j].Name
	})
	return eligible[0].HardwareAddr.String(), true
}

func isZeroMAC(mac net.HardwareAddr) bool {
	return bytes.Equal(mac, []byte{0, 0, 0, 0, 0, 0})
}

func isBroadcastMAC(mac net.HardwareAddr) bool {
	return bytes.Equal(mac, []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff})
}

// pickStableMACOrEmpty returns the primary interface MAC in lowercase
// colon-separated form (aa:bb:cc:dd:ee:ff), or "" when no stable MAC can be
// picked. Used to populate device_info.mac_address per the Sendspin spec.
func pickStableMACOrEmpty() string {
	mac, ok := pickStableMAC(allInterfaces())
	if !ok {
		return ""
	}
	return mac
}
