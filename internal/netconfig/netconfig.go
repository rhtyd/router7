package netconfig

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"net"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"

	"router7/internal/dhcp4"
	"router7/internal/dhcp6"
)

func subnetMaskSize(mask string) (int, error) {
	parts := strings.Split(mask, ".")
	if got, want := len(parts), 4; got != want {
		return 0, fmt.Errorf("unexpected number of parts in subnet mask %q: got %d, want %d", mask, got, want)
	}
	numeric := make([]byte, len(parts))
	for idx, part := range parts {
		i, err := strconv.ParseUint(part, 0, 8)
		if err != nil {
			return 0, err
		}
		numeric[idx] = byte(i)
	}
	ones, _ := net.IPv4Mask(numeric[0], numeric[1], numeric[2], numeric[3]).Size()
	return ones, nil
}

func applyDhcp4(iface, dir string) error {
	b, err := ioutil.ReadFile(filepath.Join(dir, "dhcp4/wire/lease.json"))
	if err != nil {
		return err
	}
	var got dhcp4.Config
	if err := json.Unmarshal(b, &got); err != nil {
		return err
	}

	link, err := netlink.LinkByName(iface)
	if err != nil {
		return err
	}

	subnetSize, err := subnetMaskSize(got.SubnetMask)
	if err != nil {
		return err
	}

	addr, err := netlink.ParseAddr(fmt.Sprintf("%s/%d", got.ClientIP, subnetSize))
	if err != nil {
		return err
	}

	h, err := netlink.NewHandle()
	if err != nil {
		return fmt.Errorf("netlink.NewHandle: %v", err)
	}
	if err := h.AddrAdd(link, addr); err != nil {
		return fmt.Errorf("AddrAdd(%v): %v", addr, err)
	}

	// from include/uapi/linux/rtnetlink.h
	const (
		RTPROT_STATIC = 4
		RTPROT_DHCP   = 16
	)

	if err := h.RouteAdd(&netlink.Route{
		LinkIndex: link.Attrs().Index,
		Dst: &net.IPNet{
			IP:   net.ParseIP(got.Router),
			Mask: net.CIDRMask(32, 32),
		},
		Src:      net.ParseIP(got.ClientIP),
		Scope:    netlink.SCOPE_LINK,
		Protocol: RTPROT_DHCP,
	}); err != nil {
		return fmt.Errorf("RouteAdd(router): %v", err)
	}

	if err := h.RouteAdd(&netlink.Route{
		LinkIndex: link.Attrs().Index,
		Dst: &net.IPNet{
			IP:   net.ParseIP("0.0.0.0"),
			Mask: net.CIDRMask(0, 32),
		},
		Gw:       net.ParseIP(got.Router),
		Src:      net.ParseIP(got.ClientIP),
		Protocol: RTPROT_DHCP,
	}); err != nil {
		return fmt.Errorf("RouteAdd(default): %v", err)
	}

	return nil
}

func applyDhcp6(iface, dir string) error {
	b, err := ioutil.ReadFile(filepath.Join(dir, "dhcp6/wire/lease.json"))
	if err != nil {
		return err
	}
	var got dhcp6.Config
	if err := json.Unmarshal(b, &got); err != nil {
		return err
	}

	link, err := netlink.LinkByName(iface)
	if err != nil {
		return err
	}

	for _, prefix := range got.Prefixes {
		// pick the first address of the prefix, e.g. address 2a02:168:4a00::1
		// for prefix 2a02:168:4a00::/48
		prefix.IP[len(prefix.IP)-1] = 1
		// Use the first /64 subnet within larger prefixes
		if ones, bits := prefix.Mask.Size(); ones < 64 {
			prefix.Mask = net.CIDRMask(64, bits)
		}
		addr, err := netlink.ParseAddr(prefix.String())
		if err != nil {
			return err
		}

		if err := netlink.AddrAdd(link, addr); err != nil {
			return fmt.Errorf("AddrAdd(%v): %v", addr, err)
		}
	}
	return nil
}

type InterfaceDetails struct {
	HardwareAddr string `json:"hardware_addr"` // e.g. dc:9b:9c:ee:72:fd
	Name         string `json:"name"`          // e.g. uplink0, or lan0
	Addr         string `json:"addr"`          // e.g. 192.168.42.1/24
}

type InterfaceConfig struct {
	Interfaces []InterfaceDetails `json:"interfaces"`
}

func applyInterfaces(dir string) error {
	b, err := ioutil.ReadFile(filepath.Join(dir, "interfaces.json"))
	if err != nil {
		return err
	}
	var cfg InterfaceConfig
	if err := json.Unmarshal(b, &cfg); err != nil {
		return err
	}
	byHardwareAddr := make(map[string]InterfaceDetails)
	for _, details := range cfg.Interfaces {
		byHardwareAddr[details.HardwareAddr] = details
	}
	links, err := netlink.LinkList()
	for _, l := range links {
		attr := l.Attrs()
		details, ok := byHardwareAddr[attr.HardwareAddr.String()]
		if !ok {
			continue
		}
		log.Printf("apply details %+v", details)
		ioutil.WriteFile("/dev/console", []byte(fmt.Sprintf("apply %+v\n", details)), 0600)
		if attr.Name != details.Name {
			if err := netlink.LinkSetName(l, details.Name); err != nil {
				return fmt.Errorf("LinkSetName(%q): %v", details.Name, err)
			}
			attr.Name = details.Name
		}

		if attr.OperState != netlink.OperUp {
			// Set the interface to up, which is required by all other configuration.
			if err := netlink.LinkSetUp(l); err != nil {
				return fmt.Errorf("LinkSetUp(%s): %v", attr.Name, err)
			}
		}

		if details.Addr != "" {
			addr, err := netlink.ParseAddr(details.Addr)
			if err != nil {
				return fmt.Errorf("ParseAddr(%q): %v", details.Addr, err)
			}

			if err := netlink.AddrReplace(l, addr); err != nil {
				return fmt.Errorf("AddrReplace(%s, %v): %v", attr.Name, addr, err)
			}
		}
	}
	return nil
}

func applyFirewall() error {
	// Fake it till you make it!
	// Captured via:
	// ./strace -xx -v -f -s 2048 ./xtables-multi iptables -t nat -A POSTROUTING -o uplink0 -j MASQUERADE
	optRule := "\x6e\x61\x74\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x1b\x00\x00\x00\x06\x00\x00\x00\xb8\x03\x00\x00\x00\x00\x00\x00\x98\x00\x00\x00\x00\x00\x00\x00\x30\x01\x00\x00\xc8\x01\x00\x00\x00\x00\x00\x00\x98\x00\x00\x00\x00\x00\x00\x00\x30\x01\x00\x00\x70\x02\x00\x00\x05\x00\x00\x00\x70\xed\xdb\x08\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x70\x00\x98\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x28\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\xfe\xff\xff\xff\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x70\x00\x98\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x28\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\xfe\xff\xff\xff\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x70\x00\x98\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x28\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\xfe\xff\xff\xff\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x75\x70\x6c\x69\x6e\x6b\x30\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\xff\xff\xff\xff\xff\xff\xff\xff\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x70\x00\xa8\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x38\x00\x4d\x41\x53\x51\x55\x45\x52\x41\x44\x45\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x01\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x70\x00\x98\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x28\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\xfe\xff\xff\xff\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x70\x00\xb0\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x40\x00\x45\x52\x52\x4f\x52\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x45\x52\x52\x4f\x52\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00"
	optCounters := "\x6e\x61\x74\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x06\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00"

	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_RAW, unix.IPPROTO_RAW)
	if err != nil {
		return err
	}
	// TODO: close socket later

	if err := unix.SetsockoptString(fd, unix.SOL_IP, 0x40, optRule); err != nil {
		return err
	}
	if err := unix.SetsockoptString(fd, unix.SOL_IP, 0x41, optCounters); err != nil {
		return err
	}

	return nil
}

func applySysctl() error {
	if err := ioutil.WriteFile("/proc/sys/net/ipv4/ip_forward", []byte("1"), 0644); err != nil {
		return fmt.Errorf("sysctl(net.ipv4.ip_forward=1): %v", err)
	}

	if err := ioutil.WriteFile("/proc/sys/net/ipv6/conf/all/forwarding", []byte("1"), 0644); err != nil {
		return fmt.Errorf("sysctl(net.ipv6.conf.all.forwarding=1): %v", err)
	}

	if err := ioutil.WriteFile("/proc/sys/net/ipv6/conf/uplink0/accept_ra", []byte("2"), 0644); err != nil {
		return fmt.Errorf("sysctl(net.ipv6.conf.uplink0.accept_ra=2): %v", err)
	}

	return nil
}

func Apply(iface, dir string) error {

	// TODO: split into two parts: delay the up until later
	if err := applyInterfaces(dir); err != nil {
		return err
	}

	var firstErr error

	if err := applyDhcp4(iface, dir); err != nil {
		log.Printf("cannot apply dhcp4 lease: %v", err)
		firstErr = err
	}

	if err := applyDhcp6(iface, dir); err != nil {
		log.Printf("cannot apply dhcp6 lease: %v", err)
		if firstErr == nil {
			firstErr = err
		}
	}

	if err := applySysctl(); err != nil {
		log.Printf("cannot apply sysctl config: %v", err)
		if firstErr == nil {
			firstErr = err
		}
	}

	if err := applyFirewall(); err != nil {
		return err
	}

	return firstErr
}
