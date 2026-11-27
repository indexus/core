package autoscale

import (
	"fmt"
	"sort"
	"testing"
)

// sv is one simulated vnode: its host, its share of the space and its join.
type sv struct {
	name, host string
	share      float64
	joinUntil  int
	donor      *sv
	waiting    bool
}

// simulate is a fluid replay of the 2026-11-26 campaign: four 4-vCPU
// machines of 5 slots, 4 vnodes, uniform writes ramping 2k -> 4k/s, 0.8 ms of
// CPU per write (6-16 % at 2k/s before any transfer), a donor copy of 1.5
// cores and a receiver of 1.75. A vnode asks above 500 w/s or when its host
// is over 60 %, with one request in flight; the receiver takes half after a
// 20 s join; the provisioner boots one machine per waiting requester, 90 s
// each. It returns the largest host share after t300, the peak host CPU, the
// machine count, the local splits and the host-seconds over 80 % CPU.
func simulate(place func([]MachineFacts, string, bool) Placement) (maxShare, maxCPU float64, hosts int, local int, hot int) {
	const tick, cpuPerWrite, cores, slots, cap, itemsPerShare = 2, 0.0008, 4, 5, 32, 866_000.0
	vs := []*sv{}
	hostsUp := map[string]int{} // host -> tick it is up
	for i := 0; i < 4; i++ {
		h := fmt.Sprintf("h%02d", i)
		hostsUp[h] = 0
		vs = append(vs, &sv{name: fmt.Sprint("v", i), host: h, share: .25})
	}
	booting := []int{}
	n := 4
	for t := 0; t <= 420; t += tick {
		rate := 2000.0
		if t > 120 {
			rate = min(4000, 2000+2000*float64(t-120)/60)
		}
		// finish joins
		for _, v := range vs {
			if v.donor != nil && t >= v.joinUntil {
				v.share, v.donor.share = v.donor.share/2, v.donor.share/2
				v.donor.waiting, v.donor = false, nil
			}
		}
		for i := 0; i < len(booting); {
			if t >= booting[i] {
				hostsUp[fmt.Sprintf("h%02d", len(hostsUp))] = t
				booting = append(booting[:i], booting[i+1:]...)
				continue
			}
			i++
		}
		facts := func() []MachineFacts {
			var ms []MachineFacts
			names := []string{}
			for h := range hostsUp {
				names = append(names, h)
			}
			sort.Strings(names)
			for _, h := range names {
				m := MachineFacts{Machine: h, Cores: cores, CPUOK: true, MemOK: true, MemFreePct: 80, DiskOK: true, DiskFreePct: 80, Items: map[string]int64{}}
				load := 0.0
				for _, v := range vs {
					if v.host != h {
						continue
					}
					m.Running++
					m.VNodes = append(m.VNodes, v.name)
					m.Items[v.name] = int64(v.share * itemsPerShare)
					load += v.share * rate * cpuPerWrite
					if v.donor != nil {
						m.Receiving++
						load += 1.75
					}
					if v.waiting {
						load += 1.5 // donor side of the copy: ~2 cores in the profile, less its writes
					}
				}
				m.Free = slots - m.Running
				m.CPUPct = min(100, 100*load/cores)
				maxCPU = max(maxCPU, m.CPUPct)
				ms = append(ms, m)
			}
			return ms
		}
		ms := facts()
		cpuOf := map[string]float64{}
		for _, m := range ms {
			cpuOf[m.Machine] = m.CPUPct
		}
		want := map[string]bool{}
		for _, v := range append([]*sv(nil), vs...) {
			if v.waiting || v.donor != nil || (v.share*rate < 500 && cpuOf[v.host] < 60) {
				continue
			}
			p := place(ms, v.name, len(hostsUp)+len(booting) < cap)
			if p.Machine == "" {
				if p.Kind == PlaceNew {
					want[v.name] = true
				}
				continue
			}
			if p.Kind == PlaceLocal {
				local++
			}
			r := &sv{name: fmt.Sprint("v", n), host: p.Machine, joinUntil: t + 20, donor: v}
			n++
			v.waiting = true
			vs = append(vs, r)
			ms = facts()
		}
		// The provisioner boots one machine per distinct waiting requester.
		for len(booting) < len(want) && len(hostsUp)+len(booting) < cap {
			booting = append(booting, t+90)
		}
		for _, m := range facts() {
			if m.CPUPct > 80 {
				hot += tick
			}
		}
		if t >= 300 {
			per := map[string]float64{}
			for _, v := range vs {
				per[v.host] += v.share
			}
			for _, s := range per {
				maxShare = max(maxShare, s)
			}
		}
	}
	return maxShare, maxCPU, len(hostsUp), local, hot
}

// With local first, this replay splits four times on the donor machines and
// peaks at 95 % host CPU, as the AWS run did. Placement by relief keeps every
// host under the 60 % target and spreads the space over the fleet.
func TestPlaceCampaignReplay(t *testing.T) {
	share, cpu, hosts, local, hot := simulate(Place)
	t.Logf("max host share after t300 %.1f%%, peak host CPU %.1f%%, host-seconds over 80%% %d, hosts %d, local splits %d", 100*share, cpu, hot, hosts, local)
	if local != 0 || cpu > targetCPUMaxPct || share > 1.0/8 {
		t.Fatalf("local splits %d, peak CPU %.1f%%, max host share %.1f%%", local, cpu, 100*share)
	}
}
