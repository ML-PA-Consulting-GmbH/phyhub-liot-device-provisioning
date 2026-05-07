package collector

import (
	"bytes"
	"os"
	"strings"
)

// DeviceTreeInfo holds ARM device tree identifiers.
type DeviceTreeInfo struct {
	Compatible   []string `json:"compatible,omitempty"`
	SerialNumber string   `json:"serial_number,omitempty"`
}

func collectDeviceTree() DeviceTreeInfo {
	dt := DeviceTreeInfo{}

	// compatible is a null-separated list of strings
	if data, err := os.ReadFile("/proc/device-tree/compatible"); err == nil {
		parts := bytes.Split(data, []byte{0})
		for _, p := range parts {
			s := strings.TrimSpace(string(p))
			if s != "" {
				dt.Compatible = append(dt.Compatible, s)
			}
		}
	}

	if data, err := os.ReadFile("/proc/device-tree/serial-number"); err == nil {
		dt.SerialNumber = strings.TrimRight(string(data), "\x00\n")
	}

	return dt
}
