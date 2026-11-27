//go:build ignore

// Converts `go test -bench -benchmem -count N` output (stdin) into one
// indexus-perf/1 result (stdout), the format of sdk/docs/perf.md: one
// metric per benchmark and unit, the N runs as samples.
//
//	go test -run '^$' -bench . -benchmem -count 6 ./indexus | go run scripts/perf/benchjson.go -commit abc
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"os"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
)

type metric struct {
	Name    string    `json:"name"`
	Unit    string    `json:"unit"`
	Value   float64   `json:"value"`
	Samples []float64 `json:"samples,omitempty"`
	Better  string    `json:"better"`
	Noise   *float64  `json:"noise,omitempty"`
}

type result struct {
	Schema  string         `json:"schema"`
	Suite   string         `json:"suite"`
	Commit  string         `json:"commit"`
	Branch  string         `json:"branch"`
	Date    string         `json:"date"`
	Env     map[string]any `json:"env"`
	Metrics []metric       `json:"metrics"`
}

// Kept units: time and allocations per op, and the encoded sizes some
// benchmarks report (bytes, response-bytes). Rates and ratios they also
// report are derived from these or depend on timing of the whole run.
func kept(unit string) bool {
	return unit == "ns/op" || unit == "B/op" || unit == "allocs/op" || strings.HasSuffix(unit, "bytes")
}

func median(values []float64) float64 {
	sorted := append([]float64(nil), values...)
	sort.Float64s(sorted)
	mid := len(sorted) / 2
	if len(sorted)%2 == 1 {
		return sorted[mid]
	}
	return (sorted[mid-1] + sorted[mid]) / 2
}

func quantile(sorted []float64, q float64) float64 {
	at := float64(len(sorted)-1) * q
	low := int(math.Floor(at))
	high := min(low+1, len(sorted)-1)
	return sorted[low] + (sorted[high]-sorted[low])*(at-float64(low))
}

// Same estimator as sdk/scripts/perf/result.mjs: the larger of 1.4826·MAD
// and IQR/1.349, in % of the median.
func dispersion(values []float64) float64 {
	centre := median(values)
	if len(values) < 3 || centre == 0 {
		return 0
	}
	sorted := append([]float64(nil), values...)
	sort.Float64s(sorted)
	deviations := make([]float64, len(values))
	for i, v := range values {
		deviations[i] = math.Abs(v - centre)
	}
	spread := max(1.4826*median(deviations), (quantile(sorted, 0.75)-quantile(sorted, 0.25))/1.349)
	return 100 * spread / math.Abs(centre)
}

func round(value float64) float64 {
	parsed, _ := strconv.ParseFloat(strconv.FormatFloat(value, 'g', 6, 64), 64)
	return parsed
}

func main() {
	suite := flag.String("suite", "core/go", "suite name")
	commit := flag.String("commit", "", "commit measured")
	branch := flag.String("branch", "", "branch measured")
	machine := flag.String("machine", "", "machine label (default: hostname)")
	module := flag.String("module", "github.com/indexus/go-indexus-core/", "module prefix stripped from package names")
	count := flag.Int("count", 0, "-count the benchmarks ran with")
	benchtime := flag.String("benchtime", "", "-benchtime the benchmarks ran with")
	flag.Parse()
	if *machine == "" {
		*machine, _ = os.Hostname()
	}

	// go test appends -GOMAXPROCS to every name when it is above 1.
	procs := regexp.MustCompile(fmt.Sprintf(`-%d$`, runtime.GOMAXPROCS(0)))
	env := map[string]any{
		"machine":   *machine,
		"os":        runtime.GOOS + " " + runtime.GOARCH,
		"cores":     runtime.NumCPU(),
		"runtime":   runtime.Version(),
		"count":     *count,
		"benchtime": *benchtime,
	}
	order := []string{}
	samples := map[string][]float64{}
	names := map[string][2]string{}
	pkg := ""
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 1<<20), 1<<20)
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case strings.HasPrefix(line, "pkg: "):
			pkg = strings.TrimPrefix(strings.TrimPrefix(line, "pkg: "), *module)
		case strings.HasPrefix(line, "cpu: "):
			env["cpu"] = strings.TrimPrefix(line, "cpu: ")
		case strings.HasPrefix(line, "Benchmark"):
			fields := strings.Fields(line)
			if len(fields) < 4 {
				continue
			}
			name := pkg + "/" + procs.ReplaceAllString(fields[0], "")
			for i := 2; i+1 < len(fields); i += 2 {
				value, err := strconv.ParseFloat(fields[i], 64)
				unit := fields[i+1]
				if err != nil || !kept(unit) {
					continue
				}
				key := name + " " + unit
				if _, seen := samples[key]; !seen {
					order = append(order, key)
					names[key] = [2]string{name, unit}
				}
				samples[key] = append(samples[key], value)
			}
		}
	}
	if err := scanner.Err(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if len(order) == 0 {
		fmt.Fprintln(os.Stderr, "benchjson: no benchmark line on stdin")
		os.Exit(1)
	}

	out := result{
		Schema: "indexus-perf/1",
		Suite:  *suite,
		Commit: *commit,
		Branch: *branch,
		Date:   time.Now().UTC().Format(time.RFC3339),
		Env:    env,
	}
	for _, key := range order {
		values := samples[key]
		m := metric{Name: names[key][0], Unit: names[key][1], Value: round(median(values)), Better: "lower"}
		if len(values) > 1 {
			for _, v := range values {
				m.Samples = append(m.Samples, round(v))
			}
			noise := round(dispersion(values))
			m.Noise = &noise
		}
		out.Metrics = append(out.Metrics, m)
	}
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(out); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
