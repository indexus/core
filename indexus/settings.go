package indexus

import (
	"net"
	"time"
)

type Settings struct {
	id           []byte
	name         string
	ip           string
	ips          map[string]any
	port         int
	delay        time.Duration
	memRefusePct float64
}

func NewSettings(name string, port int, delay time.Duration) (*Settings, error) {
	id, err := DecodeVnodeID(name)
	if err != nil {
		return nil, err
	}
	return &Settings{id: id, name: name, ip: "127.0.0.1", ips: getPublicIPs(), port: port, delay: delay, memRefusePct: 60}, nil
}

func (s *Settings) SetMemRefusePct(pct float64) {
	if pct > 0 {
		s.memRefusePct = pct
	}
}

func (s *Settings) SetAdvertise(addrs ...string) {
	ips := make(map[string]any, len(addrs))
	for i, a := range addrs {
		if a == "" {
			continue
		}
		ips[a] = nil
		if i == 0 {
			s.ip = a
		}
	}
	if len(ips) > 0 {
		s.ips = ips
	}
}

func getPublicIPs() map[string]any {
	ips := make(map[string]any)
	interfaces, err := net.Interfaces()
	if err != nil {
		return map[string]any{"error": err.Error()}
	}

	for _, iface := range interfaces {
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {

			var ip net.IP
			switch v := addr.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}

			if ip == nil || ip.IsLoopback() {
				continue
			}

			if ip.IsPrivate() {
				continue
			}

			ips[ip.String()] = nil
		}
	}

	return ips
}
