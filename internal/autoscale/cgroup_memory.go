package autoscale

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// /proc/meminfo describes the host, not the service's cgroup. Admission must
// respect the smaller limit or a 2 GiB service on a large host can hit OOM
// before its host-percentage guard ever refuses work.
func constrainSystemMemory(total uint64) uint64 {
	data, _ := os.ReadFile("/proc/self/cgroup")
	for _, path := range cgroupMemoryPaths(string(data)) {
		value, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		limit, err := strconv.ParseUint(strings.TrimSpace(string(value)), 10, 64)
		if err == nil && limit > 0 && (total == 0 || limit < total) {
			total = limit
		}
	}
	return total
}

func cgroupMemoryPaths(data string) []string {
	paths := []string{"/sys/fs/cgroup/memory.max", "/sys/fs/cgroup/memory/memory.limit_in_bytes"}
	for _, line := range strings.Split(data, "\n") {
		fields := strings.SplitN(line, ":", 3)
		if len(fields) != 3 {
			continue
		}
		group := strings.TrimPrefix(filepath.Clean("/"+fields[2]), "/")
		if fields[0] == "0" && fields[1] == "" {
			paths = append(paths, filepath.Join("/sys/fs/cgroup", group, "memory.max"))
		} else if strings.Contains(","+fields[1]+",", ",memory,") {
			paths = append(paths, filepath.Join("/sys/fs/cgroup/memory", group, "memory.limit_in_bytes"))
		}
	}
	return paths
}
