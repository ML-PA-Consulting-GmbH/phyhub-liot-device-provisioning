package collector

import (
	"os"
	"path/filepath"
	"strings"
)

// NetworkInterface holds the name and MAC address of a network interface.
type NetworkInterface struct {
	Name string `json:"name"`
	MAC  string `json:"mac"`
}

const netDir = "/sys/class/net/"

func collectNetwork() ([]NetworkInterface, error) {
	entries, err := os.ReadDir(netDir)
	if err != nil {
		return nil, err
	}

	var ifaces []NetworkInterface
	for _, e := range entries {
		name := e.Name()

		if !isPhysicalInterface(name) {
			continue
		}

		macPath := filepath.Join(netDir, name, "address")
		data, err := os.ReadFile(macPath)
		if err != nil {
			continue
		}

		mac := strings.TrimSpace(string(data))
		if mac == "" || mac == "00:00:00:00:00:00" {
			continue
		}

		ifaces = append(ifaces, NetworkInterface{Name: name, MAC: mac})
	}

	return ifaces, nil
}

// isPhysicalInterface returns true for hardware-backed network interfaces.
// Physical interfaces have a /sys/class/net/<name>/device symlink;
// virtual ones (veth, bridge, loopback, docker, etc.) do not.
func isPhysicalInterface(name string) bool {
	if name == "lo" {
		return false
	}
	devicePath := filepath.Join(netDir, name, "device")
	_, err := os.Stat(devicePath)
	return err == nil
}
