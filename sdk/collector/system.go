package collector

import (
	"bufio"
	"os"
	"strings"
)

type systemInfo struct {
	MachineID string
	BootID    string
	CPUInfo   CPUInfo
}

// CPUInfo holds fields parsed from /proc/cpuinfo relevant to device identity.
type CPUInfo struct {
	Model    string `json:"model,omitempty"`
	Hardware string `json:"hardware,omitempty"`
	Serial   string `json:"serial,omitempty"`
}

func collectSystem() (systemInfo, error) {
	info := systemInfo{}

	if data, err := os.ReadFile("/etc/machine-id"); err == nil {
		info.MachineID = strings.TrimSpace(string(data))
	}

	if data, err := os.ReadFile("/proc/sys/kernel/random/boot_id"); err == nil {
		info.BootID = strings.TrimSpace(string(data))
	}

	info.CPUInfo = parseCPUInfo()
	return info, nil
}

func parseCPUInfo() CPUInfo {
	f, err := os.Open("/proc/cpuinfo")
	if err != nil {
		return CPUInfo{}
	}
	defer f.Close()

	cpu := CPUInfo{}
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		key, val, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		key = strings.TrimSpace(strings.ToLower(key))
		val = strings.TrimSpace(val)

		switch key {
		case "model name", "model":
			if cpu.Model == "" {
				cpu.Model = val
			}
		case "hardware":
			cpu.Hardware = val
		case "serial":
			cpu.Serial = val
		}
	}
	return cpu
}
