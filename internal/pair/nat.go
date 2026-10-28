package pair

import (
	"context"
	"encoding/binary"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"
)

// PortMapping is a TCP port opened on the local router, so peers on the
// internet can reach this machine directly.
type PortMapping struct {
	External netip.AddrPort // what peers dial
	Method   string         // "UPnP" or "NAT-PMP"
	internal int
	renew    func(ctx context.Context) error
	remove   func(ctx context.Context) error
}

const mappingLease = time.Hour

// MapPort asks the router to forward external TCP port `want` (or another
// free one) to this machine's port `internal`. It tries NAT-PMP, then UPnP.
func MapPort(ctx context.Context, internal, want int) (*PortMapping, error) {
	gw, local, err := defaultRoute()
	if err != nil {
		return nil, err
	}
	var errs []string
	if m, err := mapNATPMP(ctx, gw, internal, want); err == nil {
		return m, nil
	} else {
		errs = append(errs, "NAT-PMP: "+err.Error())
	}
	if m, err := mapUPnP(ctx, local, internal, want); err == nil {
		return m, nil
	} else {
		errs = append(errs, "UPnP: "+err.Error())
	}
	return nil, fmt.Errorf("the router would not open a port (%s)", strings.Join(errs, "; "))
}

// Keep renews the mapping until ctx ends, then removes it.
func (m *PortMapping) Keep(ctx context.Context, logf func(string, ...any)) {
	t := time.NewTicker(mappingLease / 3)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			rctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			m.remove(rctx)
			cancel()
			return
		case <-t.C:
			if err := m.renew(ctx); err != nil && logf != nil {
				logf("renewing the router port: %v", err)
			}
		}
	}
}

// defaultRoute returns the gateway (assumed .1 of the local /24 when it
// cannot be read) and this machine's address on the default route.
func defaultRoute() (gw, local netip.Addr, err error) {
	c, err := net.Dial("udp4", "192.0.2.1:9") // no packets are sent
	if err != nil {
		return gw, local, fmt.Errorf("no network route: %w", err)
	}
	defer c.Close()
	local = c.LocalAddr().(*net.UDPAddr).AddrPort().Addr().Unmap()
	if g, ok := readGateway(); ok {
		return g, local, nil
	}
	b := local.As4()
	b[3] = 1
	return netip.AddrFrom4(b), local, nil
}

// --- NAT-PMP (RFC 6886) ---

func natpmpCall(ctx context.Context, gw netip.Addr, req []byte, respLen int) ([]byte, error) {
	c, err := net.DialUDP("udp4", nil, net.UDPAddrFromAddrPort(netip.AddrPortFrom(gw, 5351)))
	if err != nil {
		return nil, err
	}
	defer c.Close()
	buf := make([]byte, 64)
	for wait := 250 * time.Millisecond; wait <= time.Second; wait *= 2 {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if _, err := c.Write(req); err != nil {
			return nil, err
		}
		c.SetReadDeadline(time.Now().Add(wait))
		n, err := c.Read(buf)
		if err != nil {
			continue
		}
		if n < respLen || buf[1] != req[1]+128 {
			return nil, errors.New("bad reply")
		}
		if code := binary.BigEndian.Uint16(buf[2:]); code != 0 {
			return nil, fmt.Errorf("router refused (code %d)", code)
		}
		return buf[:n], nil
	}
	return nil, errors.New("no answer")
}

func mapNATPMP(ctx context.Context, gw netip.Addr, internal, want int) (*PortMapping, error) {
	r, err := natpmpCall(ctx, gw, []byte{0, 0}, 12)
	if err != nil {
		return nil, err
	}
	ext := netip.AddrFrom4([4]byte(r[8:12]))
	mapReq := func(lease uint32) ([]byte, error) {
		req := make([]byte, 12)
		req[1] = 2 // map TCP
		binary.BigEndian.PutUint16(req[4:], uint16(internal))
		binary.BigEndian.PutUint16(req[6:], uint16(want))
		binary.BigEndian.PutUint32(req[8:], lease)
		return natpmpCall(ctx, gw, req, 16)
	}
	r, err = mapReq(uint32(mappingLease / time.Second))
	if err != nil {
		return nil, err
	}
	port := binary.BigEndian.Uint16(r[10:])
	return &PortMapping{
		External: netip.AddrPortFrom(ext, port), Method: "NAT-PMP", internal: internal,
		renew:  func(context.Context) error { _, err := mapReq(uint32(mappingLease / time.Second)); return err },
		remove: func(context.Context) error { _, err := mapReq(0); return err },
	}, nil
}

// --- UPnP IGD ---

type upnpService struct {
	Type    string `xml:"serviceType"`
	Control string `xml:"controlURL"`
}

type upnpDevice struct {
	Services []upnpService `xml:"serviceList>service"`
	Devices  []upnpDevice  `xml:"deviceList>device"`
}

func (d upnpDevice) find() (upnpService, bool) {
	for _, s := range d.Services {
		if strings.Contains(s.Type, ":WANIPConnection:") || strings.Contains(s.Type, ":WANPPPConnection:") {
			return s, true
		}
	}
	for _, c := range d.Devices {
		if s, ok := c.find(); ok {
			return s, true
		}
	}
	return upnpService{}, false
}

func ssdpDiscover(ctx context.Context) (string, error) {
	c, err := net.ListenUDP("udp4", nil)
	if err != nil {
		return "", err
	}
	defer c.Close()
	dst := &net.UDPAddr{IP: net.IPv4(239, 255, 255, 250), Port: 1900}
	buf := make([]byte, 2048)
	for _, st := range []string{"urn:schemas-upnp-org:device:InternetGatewayDevice:1", "urn:schemas-upnp-org:device:InternetGatewayDevice:2"} {
		msg := "M-SEARCH * HTTP/1.1\r\nHOST: 239.255.255.250:1900\r\nMAN: \"ssdp:discover\"\r\nMX: 2\r\nST: " + st + "\r\n\r\n"
		if _, err := c.WriteTo([]byte(msg), dst); err != nil {
			return "", err
		}
		c.SetReadDeadline(time.Now().Add(2500 * time.Millisecond))
		for ctx.Err() == nil {
			n, _, err := c.ReadFrom(buf)
			if err != nil {
				break
			}
			for _, l := range strings.Split(string(buf[:n]), "\r\n") {
				if k, v, ok := strings.Cut(l, ":"); ok && strings.EqualFold(strings.TrimSpace(k), "location") {
					return strings.TrimSpace(v), nil
				}
			}
		}
	}
	return "", errors.New("no UPnP router found")
}

func soap(ctx context.Context, control, svc, action, args string) (string, error) {
	body := `<?xml version="1.0"?><s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/" s:encodingStyle="http://schemas.xmlsoap.org/soap/encoding/"><s:Body>` +
		`<u:` + action + ` xmlns:u="` + svc + `">` + args + `</u:` + action + `></s:Body></s:Envelope>`
	req, err := http.NewRequestWithContext(ctx, "POST", control, strings.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", `text/xml; charset="utf-8"`)
	req.Header.Set("SOAPAction", `"`+svc+`#`+action+`"`)
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode != 200 {
		desc := "HTTP " + resp.Status
		if i := strings.Index(string(b), "<errorCode>"); i >= 0 {
			desc = "error " + strings.SplitN(string(b[i+11:]), "<", 2)[0]
		}
		return "", fmt.Errorf("%s: %s", action, desc)
	}
	return string(b), nil
}

func xmlField(s, name string) string {
	i := strings.Index(s, "<"+name+">")
	if i < 0 {
		return ""
	}
	return strings.SplitN(s[i+len(name)+2:], "<", 2)[0]
}

func mapUPnP(ctx context.Context, local netip.Addr, internal, want int) (*PortMapping, error) {
	loc, err := ssdpDiscover(ctx)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, "GET", loc, nil)
	if err != nil {
		return nil, err
	}
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	var root struct {
		Device upnpDevice `xml:"device"`
	}
	err = xml.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&root)
	resp.Body.Close()
	if err != nil {
		return nil, fmt.Errorf("reading router description: %w", err)
	}
	svc, ok := root.Device.find()
	if !ok {
		return nil, errors.New("router has no WAN connection service")
	}
	base, _ := url.Parse(loc)
	cu, err := base.Parse(svc.Control)
	if err != nil {
		return nil, err
	}
	control := cu.String()
	r, err := soap(ctx, control, svc.Type, "GetExternalIPAddress", "")
	if err != nil {
		return nil, err
	}
	ext, err := netip.ParseAddr(xmlField(r, "NewExternalIPAddress"))
	if err != nil || !ext.Is4() {
		return nil, fmt.Errorf("router reported no usable external address (%q)", xmlField(r, "NewExternalIPAddress"))
	}
	if ext.IsPrivate() || isCGNAT(ext) {
		return nil, fmt.Errorf("router's outside address %s is itself private (another router or the ISP is in front)", ext)
	}
	add := func(port int, lease time.Duration) error {
		args := fmt.Sprintf("<NewRemoteHost></NewRemoteHost><NewExternalPort>%d</NewExternalPort><NewProtocol>TCP</NewProtocol>"+
			"<NewInternalPort>%d</NewInternalPort><NewInternalClient>%s</NewInternalClient><NewEnabled>1</NewEnabled>"+
			"<NewPortMappingDescription>RemoteSession</NewPortMappingDescription><NewLeaseDuration>%d</NewLeaseDuration>",
			port, internal, local, int(lease/time.Second))
		_, err := soap(ctx, control, svc.Type, "AddPortMapping", args)
		return err
	}
	port, lease := want, mappingLease
	err = add(port, lease)
	if err != nil && strings.Contains(err.Error(), "error 725") { // only permanent leases supported
		lease = 0
		err = add(port, lease)
	}
	for tries := 0; err != nil && strings.Contains(err.Error(), "error 718") && tries < 5; tries++ { // port taken
		port = 40000 + int(time.Now().UnixNano()%20000)
		err = add(port, lease)
	}
	if err != nil {
		return nil, err
	}
	return &PortMapping{
		External: netip.AddrPortFrom(ext, uint16(port)), Method: "UPnP", internal: internal,
		renew: func(ctx context.Context) error { return add(port, lease) },
		remove: func(ctx context.Context) error {
			_, err := soap(ctx, control, svc.Type, "DeletePortMapping",
				fmt.Sprintf("<NewRemoteHost></NewRemoteHost><NewExternalPort>%d</NewExternalPort><NewProtocol>TCP</NewProtocol>", port))
			return err
		},
	}, nil
}

func isCGNAT(a netip.Addr) bool {
	return netip.MustParsePrefix("100.64.0.0/10").Contains(a)
}

// LocalIPv4 returns this machine's address on the default route.
func LocalIPv4() (netip.Addr, error) {
	_, local, err := defaultRoute()
	return local, err
}
