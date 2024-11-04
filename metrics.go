package main

import (
	"context"
	"log"
	"net/http"
	"os/user"
	"regexp"

	systemd "github.com/coreos/go-systemd/v22/dbus"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/prometheus/procfs"
)

func MetricsHandler(pattern string, collectProc bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		registry := prometheus.NewRegistry()
		collector := NewCollector(pattern, collectProc)
		registry.MustRegister(collector)
		h := promhttp.HandlerFor(registry, promhttp.HandlerOpts{})
		h.ServeHTTP(w, r)
	}
}

var namespace = "systemd_unit"
var labels = []string{"unit", "username"}
var procLabels = []string{"unit", "username", "proc"}

const NSPerSec = 1000000000 // billion

type Collector struct {
	pattern          string
	collectProc      bool
	memoryAccounting *prometheus.Desc
	memoryMax        *prometheus.Desc
	memoryMin        *prometheus.Desc
	memoryHigh       *prometheus.Desc
	memoryLow        *prometheus.Desc
	memoryCurrent    *prometheus.Desc
	cpuAccounting    *prometheus.Desc
	cpuUsage         *prometheus.Desc
	cpuQuota         *prometheus.Desc
	procCPU          *prometheus.Desc
	procMemory       *prometheus.Desc
	procCount        *prometheus.Desc
}

type Metric struct {
	memoryMax        int64
	memoryMin        int64
	memoryHigh       int64
	memoryLow        int64
	memoryCurrent    int64
	cpuQuota         int64
	memoryAccounting bool
	cpuAccounting    bool
	cpuUsage         int64
	unit             string
	username         string
	processes        map[string]*Process
}

type Process struct {
	cpu    float64
	memory uint64
	count  uint64
}

func NewCollector(pattern string, collectProc bool) *Collector {
	return &Collector{
		pattern:     pattern,
		collectProc: collectProc,
		memoryAccounting: prometheus.NewDesc(prometheus.BuildFQName(namespace, "memory", "accounting"),
			"Whether memory accounting is enabled", labels, nil),
		memoryMax: prometheus.NewDesc(prometheus.BuildFQName(namespace, "memory", "max_bytes"),
			"Memory maximum limit", labels, nil),
		memoryMin: prometheus.NewDesc(prometheus.BuildFQName(namespace, "memory", "min_bytes"),
			"Memory minimum limit", labels, nil),
		memoryHigh: prometheus.NewDesc(prometheus.BuildFQName(namespace, "memory", "high_bytes"),
			"Memory high limit", labels, nil),
		memoryLow: prometheus.NewDesc(prometheus.BuildFQName(namespace, "memory", "low_bytes"),
			"Memory low limit", labels, nil),
		memoryCurrent: prometheus.NewDesc(prometheus.BuildFQName(namespace, "memory", "current_bytes"),
			"Resident shared size memory usage", labels, nil),
		cpuAccounting: prometheus.NewDesc(prometheus.BuildFQName(namespace, "cpu", "accounting"),
			"Whether CPU accounting is enabled", labels, nil),
		cpuUsage: prometheus.NewDesc(prometheus.BuildFQName(namespace, "cpu", "usage_ns"),
			"Total CPU usage", labels, nil),
		cpuQuota: prometheus.NewDesc(prometheus.BuildFQName(namespace, "cpu", "quota_us_per_s"),
			"CPU Quota", labels, nil),
		procCPU: prometheus.NewDesc(prometheus.BuildFQName(namespace, "proc", "cpu_usage_ns"),
			"Aggregate CPU usage for this process", procLabels, nil),
		procMemory: prometheus.NewDesc(prometheus.BuildFQName(namespace, "proc", "memory_current_bytes"),
			"Aggregate memory usage for this process", procLabels, nil),
		procCount: prometheus.NewDesc(prometheus.BuildFQName(namespace, "proc", "count"),
			"Instance count of this process", procLabels, nil),
	}
}

func (c *Collector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.memoryAccounting
	ch <- c.memoryMax
	ch <- c.memoryMin
	ch <- c.memoryHigh
	ch <- c.memoryLow
	ch <- c.memoryCurrent
	ch <- c.cpuAccounting
	ch <- c.cpuUsage
	ch <- c.cpuQuota
	if c.collectProc {
		ch <- c.procCPU
		ch <- c.procMemory
		ch <- c.procCount
	}
}

func (c *Collector) Collect(ch chan<- prometheus.Metric) {
	metrics := c.collectMetrics()
	for _, m := range metrics {
		ch <- prometheus.MustNewConstMetric(c.memoryAccounting, prometheus.GaugeValue, b2f(m.memoryAccounting), m.unit, m.username)
		ch <- prometheus.MustNewConstMetric(c.memoryMax, prometheus.GaugeValue, float64(m.memoryMax), m.unit, m.username)
		ch <- prometheus.MustNewConstMetric(c.memoryMin, prometheus.GaugeValue, float64(m.memoryMin), m.unit, m.username)
		ch <- prometheus.MustNewConstMetric(c.memoryHigh, prometheus.GaugeValue, float64(m.memoryHigh), m.unit, m.username)
		ch <- prometheus.MustNewConstMetric(c.memoryLow, prometheus.GaugeValue, float64(m.memoryLow), m.unit, m.username)
		ch <- prometheus.MustNewConstMetric(c.memoryCurrent, prometheus.GaugeValue, float64(m.memoryCurrent), m.unit, m.username)
		ch <- prometheus.MustNewConstMetric(c.cpuAccounting, prometheus.GaugeValue, b2f(m.cpuAccounting), m.unit, m.username)
		ch <- prometheus.MustNewConstMetric(c.cpuUsage, prometheus.CounterValue, float64(m.cpuUsage), m.unit, m.username)
		ch <- prometheus.MustNewConstMetric(c.cpuQuota, prometheus.CounterValue, float64(m.cpuQuota), m.unit, m.username)
		if c.collectProc {
			for name, p := range m.processes {
				ch <- prometheus.MustNewConstMetric(c.procCPU, prometheus.GaugeValue, p.cpu, m.unit, m.username, name)
				ch <- prometheus.MustNewConstMetric(c.procMemory, prometheus.GaugeValue, float64(p.memory), m.unit, m.username, name)
				ch <- prometheus.MustNewConstMetric(c.procCount, prometheus.GaugeValue, float64(p.count), m.unit, m.username, name)
			}
		}
	}
}

func (c *Collector) collectMetrics() []Metric {

	var metrics []Metric
	ctx := context.Background()
	conn, err := systemd.NewSystemConnectionContext(ctx)
	if err != nil {
		log.Println(err)
		return metrics
	}
	defer conn.Close()

	units, err := conn.ListUnitsByPatternsContext(ctx, []string{}, []string{c.pattern})
	if err != nil {
		log.Println(err)
		return metrics
	}

	for _, unit := range units {
		props, err := conn.GetUnitTypePropertiesContext(ctx, unit.Name, "Slice")
		if err != nil {
			log.Println(err)
			continue
		}
		// 'GetUnitTypePropertiesContext' may fail to read certain properties, and will return
		// a default value instead of an error. If this happens, we want to drop the metric.

		metric := Metric{
			// cast the uint64 values as int64 so the max uint64 -> -1
			memoryAccounting: props["MemoryAccounting"].(bool),
			memoryMax:        int64(props["MemoryMax"].(uint64)),
			memoryMin:        int64(props["MemoryMin"].(uint64)),
			memoryHigh:       int64(props["MemoryHigh"].(uint64)),
			memoryLow:        int64(props["MemoryLow"].(uint64)),
			memoryCurrent:    int64(props["MemoryCurrent"].(uint64)),
			cpuAccounting:    props["CPUAccounting"].(bool),
			cpuUsage:         int64(props["CPUUsageNSec"].(uint64)),
			cpuQuota:         int64(props["CPUQuotaPerSecUSec"].(uint64)),
			unit:             unit.Name,
			username:         lookupUsername(unit),
		}
		if c.collectProc {
			procs, err := collectProcesses(conn, ctx, unit.Name)
			if err != nil {
				log.Println(err)
			} else {
				metric.processes = procs
			}
		}
		metrics = append(metrics, metric)
	}
	return metrics
}

func collectProcesses(conn *systemd.Conn, ctx context.Context, unit string) (map[string]*Process, error) {
	processes := make(map[string]*Process)
	procs, err := conn.GetUnitProcesses(ctx, unit)
	if err != nil {
		return processes, err
	}

	fs, err := procfs.NewDefaultFS()
	if err != nil {
		return processes, err
	}

	for _, p := range procs {
		proc, err := fs.Proc(int(p.PID))
		if err != nil {
			log.Println(err)
			continue
		}

		comm, err := proc.Comm()
		if err != nil {
			log.Println(err)
			continue
		}

		stat, err := proc.Stat()
		if err != nil {
			log.Println(err)
			continue
		}

		smaps, err := proc.ProcSMapsRollup()
		if err != nil {
			log.Println(err)
			continue
		}

		val, ok := processes[comm]
		if !ok {
			processes[comm] = &Process{cpu: stat.CPUTime() * NSPerSec, memory: smaps.Pss, count: 1}
		} else {
			val.cpu += (stat.CPUTime() * NSPerSec)
			val.memory += smaps.Pss
			val.count += 1
		}
	}
	return processes, nil
}

func lookupUsername(unit systemd.UnitStatus) string {
	pattern := `^user-(\d+)\.slice$`
	re := regexp.MustCompile(pattern)
	match := re.FindStringSubmatch(unit.Name)

	if len(match) < 1 {
		return "unknown user"
	}

	user, err := user.LookupId(match[1])
	if err != nil {
		return "unknown user"
	}

	return user.Username
}

func b2f(b bool) float64 {
	if !b {
		return -1.0
	}
	return 1.0
}
