package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
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
	if runtime.GOOS != "linux" {
		fmt.Fprintf(os.Stderr, "[go-profile] Unsupported operating system: %s\n", runtime.GOOS)
		os.Exit(1)
	}

	if len(os.Args) < 2 || os.Args[1] == "-h" || os.Args[1] == "--help" {
		fmt.Fprintf(os.Stderr, "Usage: go-profile <command> [arguments]\n")
		os.Exit(1)
	}

	// Channel to signal when the command has finished
	done := make(chan struct{})

	// CPU usage statistics
	prev, err := getCPUTime()
	if err != nil {
		fmt.Fprintf(os.Stderr, "[go-profile] Failed to get CPU time: %s\n", err)
		os.Exit(1)
	}

	// Aggregate statistics
	totalTicks := uint64(0)
	minCpu, maxCpu, sumCpu := 100.0, 0.0, 0.0
	minRam, maxRam, sumRam := ^uint64(0), uint64(0), uint64(0)
	minGpu, maxGpu, sumGpu := 100.0, 0.0, 0.0
	hasNvidiaSmi := nvidiasmijson.HasNvidiaSmi()

	// Create the log file (append)
	log, err := os.OpenFile("go-profile.log", os.O_CREATE|os.O_APPEND|os.O_WRONLY|os.O_SYNC, 0644)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[go-profile] Failed to open log file: %s\n", err)
		os.Exit(1)
	}
	defer log.Close()

	logPrintf := func(format string, a ...interface{}) {
		str := fmt.Sprintf("[%s][go-profile] %s\n",
			time.Now().Format(time.StampMilli),
			fmt.Sprintf(format, a...))
		log.WriteString(str)
		os.Stderr.WriteString(str)
	}

	logStats := func(stats *Stats) {
		// TODO: write to a separate log JSON?
		if hasNvidiaSmi {
			logPrintf("CPU:%.2f%% | Memory:%.2f%% (%s/%s) | GPU:%.2f%%",
				stats.CpuPercent,
				stats.MemPercent,
				humanize.IBytes(stats.MemUsed),
				humanize.IBytes(stats.MemTotal),
				stats.GpuPercent,
			)
		} else {
			logPrintf("CPU:%.2f%% | Memory:%.2f%% (%s/%s)",
				stats.CpuPercent,
				stats.MemPercent,
				humanize.IBytes(stats.MemUsed),
				humanize.IBytes(stats.MemTotal),
			)
		}
	}

	log.WriteString("\n")
	logPrintf("=========================================")
	logPrintf("Starting command: %s", strings.Join(os.Args[1:], " "))

	// Start the ticker in the background
	tick := time.Millisecond * 250
	ticker := time.NewTicker(tick)
	defer ticker.Stop()

	go func() {
		for {
			select {
			case <-ticker.C:
				totalTicks++

				stats := Stats{}
				usage, err := getCPUUsage(prev)
				if err == nil {
					stats.CpuPercent = usage * 100.0
				}
				minCpu = min(minCpu, stats.CpuPercent)
				maxCpu = max(maxCpu, stats.CpuPercent)
				sumCpu += stats.CpuPercent

				memory, err := getMemoryInfo()
				if err == nil {
					used := memory.Total - memory.Available
					percent := float64(used) / float64(memory.Total) * 100.0
					stats.MemPercent = percent
					stats.MemTotal = memory.Total
					stats.MemUsed = used
				}
				minRam = min(minRam, stats.MemUsed)
				maxRam = max(maxRam, stats.MemUsed)
				sumRam += stats.MemUsed

				if hasNvidiaSmi {
					log := nvidiasmijson.XmlToObject(nvidiasmijson.RunNvidiaSmi())
					total := 0.0
					for _, gpu := range log.GPUS {
						s := strings.Split(gpu.GpuUtil, " ")
						util, err := strconv.ParseFloat(s[0], 64)
						if err != nil {
							// Pretend the GPU is at 0% utilization
							util = 0.0
						}
						total += util
					}
					stats.GpuPercent = total / float64(len(log.GPUS))
					minGpu = min(minGpu, stats.GpuPercent)
					maxGpu = max(maxGpu, stats.GpuPercent)
					sumGpu += stats.GpuPercent
				}
				logStats(&stats)

			case <-done:
				return
			}
		}
	}()

	// Collect a baseline
	logPrintf("Collecting baseline...")
	time.Sleep(time.Second + tick + 1)

	// Execute the command
	cmd := exec.Command(os.Args[1], os.Args[2:]...)

	// Create pipes to capture stdout and stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		logPrintf("Error creating stdout pipe: %v", err)
		os.Exit(1)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		logPrintf("Error creating stderr pipe: %v", err)
		os.Exit(1)
	}

	start := time.Now()
	err = cmd.Start()
	if err != nil {
		logPrintf("Failed to start command: %s", err)
		os.Exit(1)
	}

	logPrintf("Started command!")

	// Create wait group to wait for output goroutines
	var wg sync.WaitGroup

	// Handle stdout
	wg.Add(1)
	go func() {
		defer wg.Done()
		handleOutput(stdout, "stdout", os.Stdout, log)
	}()

	// Handle stderr
	wg.Add(1)
	go func() {
		defer wg.Done()
		handleOutput(stderr, "stderr", os.Stderr, log)
	}()

	// Wait for output goroutines to finish
	wg.Wait()

	// Wait for the command to finish
	err = cmd.Wait()

	// Send signal to stop the ticker
	close(done)

	// Print the total execution time
	elapsed := time.Since(start)
	logPrintf("-----------------------------------------")

	// Print the aggregate stats
	logPrintf("CPU (min: %.2f%%, max: %.2f%%, range: %.2f%%, avg: %.2f%%)",
		minCpu,
		maxCpu,
		maxCpu-minCpu,
		sumCpu/float64(totalTicks),
	)
	logPrintf("Memory (min: %s, max: %s, range: %s, avg: %s)",
		humanize.IBytes(minRam),
		humanize.IBytes(maxRam),
		humanize.IBytes(maxRam-minRam),
		humanize.IBytes(sumRam/totalTicks),
	)
	if hasNvidiaSmi {
		logPrintf("GPU (min: %.2f%%, max: %.2f%%, range: %.2f%% avg: %.2f%%)",
			minGpu,
			maxGpu,
			maxGpu-minGpu,
			sumGpu/float64(totalTicks),
		)
	}
	logPrintf("Total Execution Time: %s", elapsed)
	logPrintf("=============== FINISHED ================")

	// Check the exit code
	if err != nil {
		logPrintf("Command execution failed: %s", err)
		os.Exit(1)
	}
}

func handleOutput(output io.Reader, name string, mirror *os.File, log *os.File) {
	scanner := bufio.NewScanner(output)
	for scanner.Scan() {
		line := scanner.Text()

		timestamp := time.Now().Format(time.StampMilli)

		// Log to original output
		fmt.Fprintf(mirror, "[%s][cmd-%s] %s\n", timestamp, name, line)

		// Write to the log
		fmt.Fprintf(log, "[%s][cmd-%s] %s\n", timestamp, name, line)
	}
	if err := scanner.Err(); err != nil {
		fmt.Fprintf(log, "[go-profile] Error reading %s: %v\n", name, err)
	}
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
