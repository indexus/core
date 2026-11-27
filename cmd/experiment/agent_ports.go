package main

import (
	"errors"
	"fmt"
	"net"
	"syscall"
)

// A slot retains its endpoints across restarts. Slot zero also serves as the
// bootstrap address in the machine launch template.
type nodePorts struct{ p2p, monitor, debug int }

func (c agentConfig) portsFor(i int, slots []*slot) (nodePorts, error) {
	if c.slots != 0 || i == 0 {
		p := nodePorts{p2p: c.p2pBase + i, monitor: c.monBase + i}
		if c.debugBase > 0 {
			p.debug = c.debugBase + i
		}
		return p, nil
	}
	// Ask the kernel for free TCP ports. Hold them together until the entire
	// tuple is selected and check the matching UDP port used by HTTP/3.
	reserved := map[int]bool{c.p2pBase: true, c.monBase: true, c.debugBase: true}
	for _, s := range slots {
		if s != nil {
			reserved[s.ports.p2p], reserved[s.ports.monitor], reserved[s.ports.debug] = true, true, true
		}
	}
	var listeners []net.Listener
	defer func() {
		for _, l := range listeners {
			l.Close()
		}
	}()
	next := func(host string) (int, error) {
		for {
			l, err := net.Listen("tcp4", host+":0")
			if err != nil {
				return 0, err
			}
			listeners = append(listeners, l)
			port := l.Addr().(*net.TCPAddr).Port
			if !reserved[port] {
				return port, nil
			}
		}
	}
	p := nodePorts{}
	for {
		port, err := next("0.0.0.0")
		if err != nil {
			return p, err
		}
		u, err := net.ListenUDP("udp4", &net.UDPAddr{Port: port})
		if err != nil {
			if !errors.Is(err, syscall.EADDRINUSE) {
				return p, fmt.Errorf("checking vnode UDP port: %w", err)
			}
			continue
		}
		defer u.Close()
		p.p2p = port
		break
	}
	var err error
	if p.monitor, err = next("0.0.0.0"); err == nil && c.debugBase > 0 {
		p.debug, err = next("127.0.0.1")
	}
	return p, err
}
