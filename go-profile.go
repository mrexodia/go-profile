package main

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/dustin/go-humanize"
	nvidiasmijson "github.com/fffaraz/nvidia-smi-json"
)

type CPUTime struct {
	idle  uint64
	total uint64
}

type MemoryInfo struct {
	Total     uint64
	Free      uint64
	Available uint64
	Buffers   uint64
	Cached    uint64
}

type Stats struct {
	CpuPercent float64
	MemUsed    uint64
	MemTotal   uint64
	MemPercent float64
	GpuPercent float64
}

func main() {
	if len(os.Args) < 2 {
		fmt.Println("Usage: go-profile <command> [arguments]")
		os.Exit(1)
	}

	// Channel to signal when the command has finished
	done := make(chan struct{})

	// CPU usage statistics
	prev, err := getCPUTime()
	if err != nil {
		fmt.Printf("Failed to get CPU time: %s\n", err)
		os.Exit(1)
	}

	// Start the ticker in the background
	tick := time.Millisecond * 250
	ticker := time.NewTicker(tick)
	defer ticker.Stop()

	go func() {
		for {
			select {
			case <-ticker.C:
				stats := Stats{}
				usage, err := getCPUUsage(prev)
				if err == nil {
					stats.CpuPercent = usage * 100.0
				}

				memory, err := getMemoryInfo()
				if err == nil {
					used := memory.Total - memory.Available
					percent := float64(used) / float64(memory.Total) * 100.0
					stats.MemPercent = percent
					stats.MemTotal = memory.Total
					stats.MemUsed = used
				}

				if nvidiasmijson.HasNvidiaSmi() {
					log := nvidiasmijson.XmlToObject(nvidiasmijson.RunNvidiaSmi())
					total := 0.0
					for _, gpu := range log.GPUS {
						s := strings.Split(gpu.GpuUtil, " ")
						util, err := strconv.ParseFloat(s[0], 64)
						if err != nil {
							fmt.Printf("Bad")
						}
						total += util
					}
					stats.GpuPercent = total / float64(len(log.GPUS))
				}

				fmt.Fprintf(os.Stderr, "[go-profile] CPU:%.2f%% | Memory:%.2f%% (%s/%s) | GPU:%.2f%%\n",
					stats.CpuPercent,
					stats.MemPercent,
					humanize.IBytes(stats.MemUsed),
					humanize.IBytes(stats.MemTotal),
					stats.GpuPercent)

			case <-done:
				return
			}
		}
	}()

	// Collect a baseline
	fmt.Fprintf(os.Stderr, "[go-profile] Collecting baseline...\n")
	time.Sleep(time.Second + tick + 1)

	// Execute the command
	cmd := exec.Command(os.Args[1], os.Args[2:]...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	start := time.Now()
	err = cmd.Start()
	if err != nil {
		fmt.Printf("Failed to start command: %s\n", err)
		os.Exit(1)
	}

	fmt.Fprintf(os.Stderr, "[go-profile] Started command!\n")

	// Wait for the command to finish
	err = cmd.Wait()
	if err != nil {
		fmt.Printf("Command execution failed: %s\n", err)
		os.Exit(1)
	}

	// Send signal to stop the ticker
	close(done)

	// Print the total execution time
	elapsed := time.Since(start)
	fmt.Printf("Total Execution Time: %s\n", elapsed)
}

/*
	References:

- https://colby.id.au/calculating-cpu-usage-from-proc-stat/
- https://www.kernel.org/doc/Documentation/filesystems/proc.txt
*/
func getCPUTime() (*CPUTime, error) {
	// Read the procfile
	data, err := os.ReadFile("/proc/stat")
	if err != nil {
		return nil, err
	}

	// Get the fields from the first line
	lines := strings.Split(string(data), "\n")
	fields := strings.Fields(lines[0])

	// Get the idle time
	idle, err := strconv.ParseUint(fields[4], 10, 64)
	if err != nil {
		return nil, err
	}

	// Get the total time
	result := &CPUTime{idle: idle, total: 0}
	for _, field := range fields[1:] {
		value, err := strconv.ParseUint(field, 10, 64)
		if err != nil {
			return nil, err
		}
		result.total += value
	}

	return result, nil
}

func getCPUUsage(prev *CPUTime) (float64, error) {
	// Get CPU times
	stats, err := getCPUTime()
	if err != nil {
		return 0, err
	}

	// Calculate the usage
	diffIdle := float64(stats.idle - prev.idle)
	diffTotal := float64(stats.total - prev.total)
	usage := (diffTotal - diffIdle) / diffTotal

	// Update the previous data
	*prev = *stats

	return usage, nil
}

func getMemoryInfo() (MemoryInfo, error) {
	memInfo := MemoryInfo{}

	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return memInfo, err
	}

	lines := strings.Split(string(data), "\n")
	for _, line := range lines {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		key := fields[0]
		value, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			return memInfo, err
		}
		switch key {
		case "MemTotal:":
			memInfo.Total = value * 1024
		case "MemFree:":
			memInfo.Free = value * 1024
		case "MemAvailable:":
			memInfo.Available = value * 1024
		case "Buffers:":
			memInfo.Buffers = value * 1024
		case "Cached:":
			memInfo.Cached = value * 1024
		}
	}

	return memInfo, nil
}
