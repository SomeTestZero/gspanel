package main

import (
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// ---------- 监控：/proc 读取系统与实例资源，无外部依赖 ----------

type SystemStats struct {
	Load1       float64 `json:"load1"`
	Load5       float64 `json:"load5"`
	Load15      float64 `json:"load15"`
	CPUCores    int     `json:"cpu_cores"`
	MemTotal    uint64  `json:"mem_total"`
	MemAvail    uint64  `json:"mem_avail"`
	MemUsedPct  float64 `json:"mem_used_pct"`
	SwapTotal   uint64  `json:"swap_total"`
	SwapFree    uint64  `json:"swap_free"`
	DiskTotal   uint64  `json:"disk_total"`
	DiskFree    uint64  `json:"disk_free"`
	DiskUsedPct float64 `json:"disk_used_pct"`
	Uptime      string  `json:"uptime"`
}

type InstanceStats struct {
	CPUPercent float64 `json:"cpu_percent"` // 单核百分比，可超 100
}

type Monitor struct {
	state *State
	mu    sync.RWMutex
	cpu   map[string]float64 // instance -> cpu%
	last  map[string]sample
}

type sample struct {
	nsec uint64
	at   time.Time
}

func NewMonitor(state *State) *Monitor {
	return &Monitor{state: state, cpu: map[string]float64{}, last: map[string]sample{}}
}

func (m *Monitor) Start() {
	go func() {
		ticker := time.NewTicker(3 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			m.sampleAll()
		}
	}()
}

func (m *Monitor) sampleAll() {
	m.state.mu.RLock()
	insts := make([]*Instance, 0, len(m.state.Instances))
	for _, inst := range m.state.Instances {
		insts = append(insts, inst)
	}
	m.state.mu.RUnlock()

	for _, inst := range insts {
		st := serviceStatus(inst)
		m.mu.Lock()
		if st.ActiveState == "active" && st.CPUUsageNSec > 0 {
			if prev, ok := m.last[inst.Name]; ok {
				dNsec := float64(st.CPUUsageNSec - prev.nsec)
				dWall := float64(time.Since(prev.at).Nanoseconds())
				if dWall > 0 {
					m.cpu[inst.Name] = dNsec / dWall * 100
				}
			}
			m.last[inst.Name] = sample{nsec: st.CPUUsageNSec, at: time.Now()}
		} else {
			m.cpu[inst.Name] = 0
			delete(m.last, inst.Name)
		}
		m.mu.Unlock()
	}
}

func (m *Monitor) InstanceCPU(name string) float64 {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.cpu[name]
}

func ReadSystemStats() *SystemStats {
	st := &SystemStats{CPUCores: numCPU()}

	if data, err := os.ReadFile("/proc/loadavg"); err == nil {
		f := strings.Fields(string(data))
		if len(f) >= 3 {
			st.Load1, _ = strconv.ParseFloat(f[0], 64)
			st.Load5, _ = strconv.ParseFloat(f[1], 64)
			st.Load15, _ = strconv.ParseFloat(f[2], 64)
		}
	}

	var memTotal, memAvail, swapTotal, swapFree uint64
	if data, err := os.ReadFile("/proc/meminfo"); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			f := strings.Fields(line)
			if len(f) < 2 {
				continue
			}
			v, _ := strconv.ParseUint(f[1], 10, 64)
			v *= 1024 // kB -> B
			switch strings.TrimSuffix(f[0], ":") {
			case "MemTotal":
				memTotal = v
			case "MemAvailable":
				memAvail = v
			case "SwapTotal":
				swapTotal = v
			case "SwapFree":
				swapFree = v
			}
		}
	}
	st.MemTotal, st.MemAvail = memTotal, memAvail
	st.SwapTotal, st.SwapFree = swapTotal, swapFree
	if memTotal > 0 {
		st.MemUsedPct = float64(memTotal-memAvail) / float64(memTotal) * 100
	}

	var fs syscall.Statfs_t
	if syscall.Statfs("/", &fs) == nil {
		st.DiskTotal = fs.Blocks * uint64(fs.Bsize)
		st.DiskFree = fs.Bavail * uint64(fs.Bsize)
		if st.DiskTotal > 0 {
			st.DiskUsedPct = float64(st.DiskTotal-st.DiskFree) / float64(st.DiskTotal) * 100
		}
	}

	if data, err := os.ReadFile("/proc/uptime"); err == nil {
		f := strings.Fields(string(data))
		if len(f) > 0 {
			if secs, err := strconv.ParseFloat(f[0], 64); err == nil {
				d := time.Duration(secs) * time.Second
				st.Uptime = d.Truncate(time.Minute).String()
			}
		}
	}
	return st
}

func numCPU() int {
	data, err := os.ReadFile("/proc/cpuinfo")
	if err != nil {
		return 1
	}
	n := 0
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "processor") {
			n++
		}
	}
	if n == 0 {
		return 1
	}
	return n
}
