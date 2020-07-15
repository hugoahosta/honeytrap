// Package nscanary provides canary listener using gvisors netstack.
// https://github.com/google/gvisor/tree/master/pkg/tcpip
package nscanary

//
// config.toml
//  listener="canary-netstack"
//  interfaces=["iface"]
//  addr=""
//

import (
	"context"
	"fmt"
	"net"
	"strings"
	"syscall"

	"github.com/honeytrap/honeytrap/event"
	"github.com/honeytrap/honeytrap/listener"
	"github.com/honeytrap/honeytrap/pushers"
	"github.com/op/go-logging"
	"github.com/vishvananda/netlink"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/link/fdbased"
	"gvisor.dev/gvisor/pkg/tcpip/link/rawfile"
	"gvisor.dev/gvisor/pkg/tcpip/link/tun"
	"gvisor.dev/gvisor/pkg/tcpip/network/arp"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv6"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/icmp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/raw"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"
)

var log = logging.MustGetLogger("listeners/netstack-canary")

var (
	_                    = listener.Register("netstack-canary", New)
	EventCategoryUnknown = event.Category("unknown")
	SensorCanary         = event.Sensor("canary")

	CanaryOptions = event.NewWith(
		SensorCanary,
	)
)

type Canary struct {
	Addr               string `toml:"addr"`
	Addresses          []net.Addr
	Interfaces         []string `toml:"interfaces"`
	TransportProtocols []string `toml:"transport_protocols"`

	transportProtos []stack.TransportProtocol
	events          pushers.Channel
	nconn           chan net.Conn
	knockChan       chan KnockGrouper

	stack *stack.Stack
}

//AddAddress implements listener.AddAddresser
func (c *Canary) AddAddress(a net.Addr) {
	c.Addresses = append(c.Addresses, a)
}

func New(options ...func(listener.Listener) error) (listener.Listener, error) {
	c := &Canary{
		events:    pushers.MustDummy(),
		knockChan: make(chan KnockGrouper),
	}

	for _, opt := range options {
		if err := opt(c); err != nil {
			return nil, err
		}
	}

	if len(c.Interfaces) == 0 {
		return nil, fmt.Errorf("no interface defined")
	}

	if protos, err := getTransportProtos(c.TransportProtocols); err != nil {
		return nil, err
	} else {
		c.transportProtos = protos
	}

	iface := c.Interfaces[0]

	ifaceLink, err := netlink.LinkByName(iface)
	if err != nil {
		return nil, fmt.Errorf("unable to find %s: %v", iface, err)
	}

	// create a new stack
	opts := stack.Options{
		NetworkProtocols:   []stack.NetworkProtocol{ipv4.NewProtocol(), ipv6.NewProtocol(), arp.NewProtocol()},
		TransportProtocols: []stack.TransportProtocol{icmp.NewProtocol4(), icmp.NewProtocol6(), udp.NewProtocol(), tcp.NewProtocol()},
		RawFactory:         raw.EndpointFactory{},
	}

	s := stack.New(opts)

	// setup a link endpoint

	mtu, err := rawfile.GetMTU(iface)
	if err != nil {
		return nil, err
	}

	fd, err := fileDescriptor(iface, ifaceLink.Attrs().Index)
	if err != nil {
		return nil, err
	}

	linkEP, err := fdbased.New(&fdbased.Options{
		FDs:            []int{fd},
		MTU:            mtu,
		EthernetHeader: true,
		Address:        tcpip.LinkAddress(ifaceLink.Attrs().HardwareAddr),
		ClosedFunc: func(e *tcpip.Error) {
			if e != nil {
				log.Errorf("File descriptor closed: %v", err)
			}
		},
	})
	if err != nil {
		return nil, fmt.Errorf("failed creating a link endpoint: %w", err)
	}

	s.CreateNIC(1, NewWrapper(linkEP, c.events, c.knockChan))

	// set the route table.

	link := ifaceLink

	routes, err := Routes(link)
	if err != nil {
		return nil, fmt.Errorf("get routes: %w", err)
	}
	s.SetRouteTable(routes)

	// stack.AddAddress()

	addrs, err := netlink.AddrList(link, netlink.FAMILY_ALL)
	if err != nil {
		return nil, fmt.Errorf("error retrieving interface ip addresses: %s", err.Error())
	}

	if c.Addr != "" {
		addr, err := netlink.ParseAddr(c.Addr)
		if err != nil {
			return nil, fmt.Errorf("bad IP address: %v: %s", c.Addr, err)
		}
		addrs = []netlink.Addr{*addr}
	}

	for _, parsedAddr := range addrs {
		var addr tcpip.Address
		var proto tcpip.NetworkProtocolNumber

		if _, bits := parsedAddr.Mask.Size(); bits == 32 {
			addr = tcpip.Address(parsedAddr.IP)
			proto = ipv4.ProtocolNumber
		} else if _, bits := parsedAddr.Mask.Size(); bits == 256 {
			addr = tcpip.Address(parsedAddr.IP)
			proto = ipv6.ProtocolNumber
		} else {
			return nil, fmt.Errorf("unknown IP type: %v, bits=%d", c.Addr, bits)
		}

		log.Debugf("Listening on: %s (%d)\n", parsedAddr.String(), proto)

		//stack.AddAddressRange() from subnet??
		if err := s.AddAddress(1, proto, addr); err != nil {
			return nil, fmt.Errorf(err.String())
		}
	}

	s.SetSpoofing(1, true)

	fmt.Printf("s.GetRouteTable() = %+v\n", s.GetRouteTable())
	fmt.Printf("s.NICInfo() = %+v\n", s.NICInfo())

	c.stack = s

	return c, nil
}

func (c *Canary) Accept() (net.Conn, error) {
	conn := <-c.nconn
	return conn, nil
}

func (c *Canary) SetChannel(ch pushers.Channel) {
	c.events = ch
}

func parseAddr(address net.Addr, nic tcpip.NICID) (tcpip.FullAddress, tcpip.NetworkProtocolNumber) {
	proto := ipv4.ProtocolNumber

	full := tcpip.FullAddress{
		NIC: nic,
	}

	if a, ok := address.(*net.TCPAddr); ok {
		full.Addr = tcpip.Address(a.IP)
		full.Port = uint16(a.Port)
	} else if a, ok := address.(*net.UDPAddr); ok {
		full.Addr = tcpip.Address(a.IP)
		full.Port = uint16(a.Port)
	}

	if full.Addr.To4() == "" {
		proto = ipv6.ProtocolNumber
	}

	return full, proto
}

func (c *Canary) Start(ctx context.Context) error {
	go RunKnockDetector(ctx, c.knockChan, c.events)

	for _, address := range c.Addresses {
		fa, netproto := parseAddr(address, 1)

		if _, ok := address.(*net.TCPAddr); ok {
			l, err := gonet.ListenTCP(c.stack, fa, netproto)
			if err != nil {
				log.Errorf("Error starting listener: %v", err)
				continue
			}

			log.Infof("Listener started: tcp/%s", address)

			go func() {
				for {
					conn, err := l.Accept()
					if err != nil {
						log.Errorf("Error accepting connection: %s", err.Error())
						continue
					}

					c.nconn <- conn
				}
			}()
		} else if _, ok := address.(*net.UDPAddr); ok {
			l, err := gonet.DialUDP(c.stack, &fa, nil, netproto)
			if err != nil {
				log.Errorf("Error starting listener: %v", err)
				continue
			}

			ul := UDPConn{l}

			log.Infof("Listener started: udp/%s", address)

			go func() {
				for {
					var buf [65535]byte

					n, raddr, err := ul.ReadFromUDP(buf[:])
					if err != nil {
						log.Error("Error reading udp:", err.Error())
						continue
					}

					c.nconn <- &listener.DummyUDPConn{
						Buffer: buf[:n],
						Laddr:  l.LocalAddr(),
						Raddr:  raddr,
						Fn:     ul.WriteToUDP,
					}
				}
			}()
		}
	}
	return nil
}

//UDPConn extends gonet.UDPConn.
type UDPConn struct {
	*gonet.UDPConn
}

//WriteToUDP satifies listener.DummyUDPConn.Fn
func (c *UDPConn) WriteToUDP(b []byte, addr *net.UDPAddr) (int, error) {
	return c.WriteTo(b, net.Addr(addr))
}

func (c *UDPConn) ReadFromUDP(b []byte) (int, *net.UDPAddr, error) {
	n, addr, err := c.ReadFrom(b)
	return n, addr.(*net.UDPAddr), err
}

func Routes(link netlink.Link) ([]tcpip.Route, error) {
	rs, err := netlink.RouteList(link, netlink.FAMILY_ALL)
	if err != nil {
		return nil, fmt.Errorf("error getting routes from %q: %v", link.Attrs().Name, err)
	}

	var (
		subnet tcpip.Subnet
		routes = make([]tcpip.Route, 0, len(rs))
	)

	for _, route := range rs {
		if route.Dst == nil && route.Gw != nil { //default route.
			if route.Gw.To4() == nil {
				subnet, err = tcpip.NewSubnet(ipToAddress(net.IPv6zero), tcpip.AddressMask(net.IPv6zero))
			} else {
				subnet, err = tcpip.NewSubnet(ipToAddress(net.IPv4zero), tcpip.AddressMask(net.IPv4zero))
			}
		} else {
			subnet, err = tcpip.NewSubnet(ipToAddress(route.Dst.IP.Mask(route.Dst.Mask)), tcpip.AddressMask(route.Dst.Mask))
		}
		if err != nil {
			return nil, err
		}
		routes = append(routes, tcpip.Route{
			Destination: subnet,
			NIC:         1,
		})
	}
	return routes, nil
}

// ipToAddressAndProto converts IP to tcpip.Address and a protocol number.
//
// Note: don't use 'len(ip)' to determine IP version because length is always 16.
func ipToAddressAndProto(ip net.IP) (tcpip.NetworkProtocolNumber, tcpip.Address) {
	if i4 := ip.To4(); i4 != nil {
		return ipv4.ProtocolNumber, tcpip.Address(i4)
	}
	return ipv6.ProtocolNumber, tcpip.Address(ip)
}

// ipToAddress converts IP to tcpip.Address, ignoring the protocol.
func ipToAddress(ip net.IP) tcpip.Address {
	_, addr := ipToAddressAndProto(ip)
	return addr
}

func htons(n uint16) uint16 {
	var (
		high = n >> 8
		ret  = n<<8 + high
	)
	return ret
}

//fileDescriptor opens a raw socket and binds it to network interface with name 'link'
//returns the socket file descriptor, on error fd=0.
func fileDescriptor(link string, linkIndex int) (int, error) {

	var fd int
	var err error

	if strings.HasPrefix(link, "tun") {
		fd, err = tun.Open(link)
		if err != nil {
			return 0, fmt.Errorf("could not open tun interface: %s", err.Error())
		}
	} else if strings.HasPrefix(link, "tap") {
		fd, err = tun.OpenTAP(link)
		if err != nil {
			return 0, fmt.Errorf("could not open tap interface: %s", err.Error())
		}
	} else {
		fd, err = syscall.Socket(syscall.AF_PACKET, syscall.SOCK_RAW, int(htons(syscall.ETH_P_ALL)))
		if err != nil {
			return 0, fmt.Errorf("could not create socket: %s", err.Error())
		}

		if fd < 0 {
			return 0, fmt.Errorf("socket error: fd < 0")
		}

		ll := syscall.SockaddrLinklayer{
			Protocol: htons(syscall.ETH_P_ALL),
			Ifindex:  linkIndex,
			Hatype:   0, // No ARP type.
			Pkttype:  syscall.PACKET_HOST,
		}

		if err := syscall.Bind(fd, &ll); err != nil {
			return 0, fmt.Errorf("unable to bind to %q: %v", link, err)
		}
	}
	return fd, nil
}

func getTransportProtos(protos []string) ([]stack.TransportProtocol, error) {
	pp := make([]stack.TransportProtocol, 0, 4)

	if len(protos) == 0 {
		//use all transport protocols.
		protos = []string{"tcp", "udp", "icmp4", "icmp6"}
	}

	for _, name := range protos {
		switch name {
		case "tcp":
			pp = append(pp, tcp.NewProtocol())
		case "udp":
			pp = append(pp, udp.NewProtocol())
		case "icmp4":
			pp = append(pp, icmp.NewProtocol4())
		case "icmp6":
			pp = append(pp, icmp.NewProtocol6())
		default:
			return nil, fmt.Errorf("unknown transport protocol: %s", name)
		}
	}

	return pp, nil
}
