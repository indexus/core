package autoscale

import (
	"bufio"
	"io"
	"log/slog"
	"os"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

type ResourceSample struct {
	HeapInuse    uint64  `json:"heap_inuse"`
	RSS          uint64  `json:"rss_bytes"`
	NonFileRSS   uint64  `json:"non_file_rss_bytes"`
	FileRSS      uint64  `json:"file_rss_bytes"`
	MemTotal     uint64  `json:"mem_total_bytes"`
	MemAvailable uint64  `json:"mem_available_bytes"`
	MemPct       float64 `json:"mem_pct"`
	DiskPath     string  `json:"disk_path,omitempty"`
	DiskFreePct  float64 `json:"disk_free_pct"`
	DiskOK       bool    `json:"disk_ok"`

	// CPUPct is the CPU pressure the controller evaluates: the busier of this
	// process and its host, so that vnodes sharing a machine see the machine.
	CPUPct float64 `json:"cpu_pct"`
	CPUOK  bool    `json:"cpu_ok"`
	// ProcessCPUPct is this process alone, over all the cores of the host.
	ProcessCPUPct float64 `json:"cpu_process_pct"`
	// HostCPUPct is the whole machine (/proc/stat); HostCPUOK is false where
	// it cannot be read, and CPUPct is then the process value.
	HostCPUPct    float64 `json:"cpu_host_pct"`
	HostCPUOK     bool    `json:"cpu_host_ok"`
	HostIOWaitPct float64 `json:"cpu_host_iowait_pct"`
	// ProcessMemPct and HostMemPct are the two terms MemPct is the larger of.
	ProcessMemPct float64 `json:"mem_process_pct"`
	HostMemPct    float64 `json:"mem_host_pct"`
	HostMemOK     bool    `json:"mem_host_ok"`
}

// Sampler reads the resources of this process and of its host.
type Sampler func(diskPath string) ResourceSample

func SampleResources(diskPath string) ResourceSample {
	s := SampleCapacity(diskPath)
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	s.HeapInuse = ms.HeapInuse
	process, processOK := cpu.sample()
	host, hostOK := hostCPU.sample()
	s.setCPU(process, processOK, host, hostOK)
	return s
}

// SampleCapacity reads memory and disk independently of the caller’s CPU meter.
func SampleCapacity(diskPath string) ResourceSample {
	s := sampleMemory()
	s.DiskPath, s.DiskFreePct, s.DiskOK = diskPath, 100, true
	if diskPath != "" {
		if free, ok := diskFreePct(diskPath); ok {
			s.DiskFreePct = free
			s.DiskOK = true
		} else {
			s.DiskOK = false
		}
	}
	return s
}

// setCPU records the process and host readings and the pressure evaluated
// from them. Without a host reading the process stands for it.
func (s *ResourceSample) setCPU(process float64, processOK bool, host hostCPUSample, hostOK bool) {
	s.ProcessCPUPct = process
	s.CPUPct, s.CPUOK = process, processOK
	if !hostOK {
		return
	}
	s.HostCPUPct, s.HostIOWaitPct, s.HostCPUOK = host.busyPct, host.ioWaitPct, true
	s.CPUPct, s.CPUOK = max(process, host.busyPct), true
}

type cpuMeter struct {
	mu   sync.Mutex
	at   time.Time
	used time.Duration
}

var cpu = &cpuMeter{}

func (m *cpuMeter) sample() (float64, bool) {
	var usage syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &usage); err != nil {
		return 0, false
	}
	used := time.Duration(usage.Utime.Nano() + usage.Stime.Nano())
	now := time.Now()

	m.mu.Lock()
	defer m.mu.Unlock()

	previousAt, previousUsed := m.at, m.used
	m.at, m.used = now, used
	if previousAt.IsZero() {
		return 0, false
	}
	elapsed := now.Sub(previousAt)
	cores := float64(runtime.NumCPU())
	if elapsed <= 0 || cores <= 0 {
		return 0, false
	}
	return 100 * float64(used-previousUsed) / (float64(elapsed) * cores), true
}

// hostCPUMeter measures the whole machine between two reads of /proc/stat.
//
// Busy is everything but idle: user, nice, system, irq, softirq, and also
// iowait and steal. The placer counts the same way (cmd/experiment,
// hostCPUMeter), and the two must judge one machine alike. Stolen time is
// capacity this host does not have; a CPU waiting on I/O is not headroom for
// a store whose writers are the ones waiting. iowait is also reported apart.
type hostCPUMeter struct {
	mu      sync.Mutex
	path    string
	last    HostCPUTicks
	started bool
}

// HostCPUTicks groups Linux aggregate CPU counters into disjoint categories.
type HostCPUTicks struct {
	UserNice, System, IRQSoftIRQ, IOWait, Steal, Idle uint64
	Complete                                          bool // the kernel supplied all eight base counters
}

func (t HostCPUTicks) Total() uint64 {
	return t.UserNice + t.System + t.IRQSoftIRQ + t.IOWait + t.Steal + t.Idle
}

type hostCPUSample struct{ busyPct, ioWaitPct float64 }

var hostCPU = &hostCPUMeter{path: "/proc/stat"}

func (m *hostCPUMeter) sample() (hostCPUSample, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	raw, err := os.ReadFile(m.path)
	if err != nil {
		m.started = false
		return hostCPUSample{}, false
	}
	return m.observe(string(raw))
}

// observe folds one read of /proc/stat. The first read, a counter that went
// back and a read with no tick elapsed give no sample.
func (m *hostCPUMeter) observe(raw string) (hostCPUSample, bool) {
	now, ok := ParseHostCPUTicks(raw)
	if !ok {
		m.started = false
		return hostCPUSample{}, false
	}
	previous, started := m.last, m.started
	m.last, m.started = now, true
	if !started || now.Total() <= previous.Total() || now.Idle < previous.Idle || now.IOWait < previous.IOWait {
		return hostCPUSample{}, false
	}
	total, idle, ioWait := now.Total()-previous.Total(), now.Idle-previous.Idle, now.IOWait-previous.IOWait
	if idle > total {
		return hostCPUSample{}, false
	}
	return hostCPUSample{
		busyPct:   100 * float64(total-idle) / float64(total),
		ioWaitPct: 100 * float64(min(ioWait, total)) / float64(total),
	}, true
}

// ParseHostCPUTicks reads the aggregate line. The kernel already includes
// guest and guest_nice in user and nice.
func ParseHostCPUTicks(raw string) (HostCPUTicks, bool) {
	line, _, _ := strings.Cut(raw, "\n")
	fields := strings.Fields(line)
	if len(fields) < 5 || fields[0] != "cpu" {
		return HostCPUTicks{}, false
	}
	var ticks [8]uint64
	for i, field := range fields[1:min(len(fields), 9)] {
		v, err := strconv.ParseUint(field, 10, 64)
		if err != nil {
			return HostCPUTicks{}, false
		}
		ticks[i] = v
	}
	return HostCPUTicks{
		UserNice: ticks[0] + ticks[1], System: ticks[2], Idle: ticks[3],
		IOWait: ticks[4], IRQSoftIRQ: ticks[5] + ticks[6], Steal: ticks[7],
		Complete: len(fields) >= 9,
	}, true
}

func CapHeapToHost() {
	if os.Getenv("GOMEMLIMIT") != "" {
		return
	}
	total := systemMemTotal()
	limit := defaultHeapLimit(total)
	debug.SetMemoryLimit(limit)
	slog.Info("heap capped to host memory", "limit_bytes", limit, "host_bytes", total)
}

// Leave headroom for mapped file pages, stacks, networking and OS page cache.
// On small hosts (<= 3 GiB) cap at 75% (up to 1.5 GiB); on larger hosts use
// 50% of host RAM so Go GC does not thrash under multi-million-item loads.
// GOMEMLIMIT remains an explicit override.
func defaultHeapLimit(total uint64) int64 {
	const fallback = int64(1536 << 20)
	if total == 0 {
		return fallback
	}
	if half := int64(total / 2); half > fallback {
		return half
	}
	return min(fallback, int64(total/4*3))
}

// HeapShareOfHost is the heap limit of one of the node processes that share
// this machine. Each used to cap itself at half the host, so five of them
// together aimed at two and a half times its memory (read in code
// 2026-11-26; not the cause of a measured failure). They now split what one
// process alone would take.
func HeapShareOfHost(processes int) int64 {
	return heapShare(systemMemTotal(), processes)
}

func heapShare(total uint64, processes int) int64 {
	alone := defaultHeapLimit(total)
	// Below this a Go heap spends its time collecting; the machine then
	// has too little memory for that many processes, which its placement
	// floor on available memory refuses, not this limit.
	const least = int64(256 << 20)
	return min(alone, max(least, alone/int64(max(1, processes))))
}

func MemoryUsedPct() float64 {
	return sampleMemory().MemPct
}

func sampleMemory() ResourceSample {
	process, host := readProcBytes("/proc/self/status"), readProcBytes("/proc/meminfo")
	return memoryPressure(process, host, constrainSystemMemory(host["MemTotal"]))
}

// File-backed mmap pages are reclaimable. Keep raw RSS observable, but use
// anonymous/shared memory and actual host pressure for admission and scaling.
func memoryPressure(process, host map[string]uint64, limit uint64) ResourceSample {
	s := ResourceSample{RSS: process["VmRSS"], FileRSS: process["RssFile"], MemTotal: limit, MemAvailable: host["MemAvailable"]}
	if anon, known := process["RssAnon"]; known {
		s.NonFileRSS = anon + process["RssShmem"]
	} else {
		s.NonFileRSS = s.RSS // Conservative if the kernel breakdown is unavailable.
	}
	if limit > 0 {
		s.ProcessMemPct = 100 * float64(s.NonFileRSS) / float64(limit)
	}
	s.MemPct = s.ProcessMemPct
	if available, known := host["MemAvailable"]; known && host["MemTotal"] > 0 {
		total := host["MemTotal"]
		s.HostMemPct, s.HostMemOK = 100*float64(total-min(total, available))/float64(total), true
		s.MemPct = max(s.MemPct, s.HostMemPct)
	}
	return s
}

func readProcBytes(path string) map[string]uint64 {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	return parseProcBytes(f)
}

func parseProcBytes(r io.Reader) map[string]uint64 {
	values := map[string]uint64{}
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) != 3 || fields[2] != "kB" {
			continue
		}
		kb, err := strconv.ParseUint(fields[1], 10, 64)
		if err == nil {
			values[strings.TrimSuffix(fields[0], ":")] = kb * 1024
		}
	}
	return values
}

func systemMemTotal() uint64 {
	return constrainSystemMemory(readProcBytes("/proc/meminfo")["MemTotal"])
}

func diskFreePct(path string) (float64, bool) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, false
	}
	total := st.Blocks * uint64(st.Bsize)
	if total == 0 {
		return 0, false
	}
	free := st.Bavail * uint64(st.Bsize)
	return 100 * float64(free) / float64(total), true
}
