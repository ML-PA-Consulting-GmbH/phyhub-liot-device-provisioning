package collector

import (
	"fmt"
	"io"
)

// HardwareInfo holds all collected device identifiers.
// Every field is best-effort: missing data is represented by zero values.
type HardwareInfo struct {
	MachineID         string             `json:"machine_id,omitempty"`
	BootID            string             `json:"boot_id,omitempty"`
	CPUInfo           CPUInfo            `json:"cpu_info,omitempty"`
	DeviceTree        DeviceTreeInfo     `json:"device_tree,omitempty"`
	DMI               DMIInfo            `json:"dmi,omitempty"`
	TPM               TPMInfo            `json:"tpm"`
	SecureBoot        SecureBootInfo     `json:"secure_boot"`
	NetworkInterfaces []NetworkInterface `json:"network_interfaces,omitempty"`
}

// Option configures Collect. Options are applied left-to-right; later
// options override earlier ones.
type Option func(*options)

type options struct {
	out io.Writer
}

// WithOutput directs per-source warnings (e.g. "system info: ...") to w.
// When unset, warnings are discarded so a library caller is never spammed
// by stderr it didn't ask for. The provisioning tool passes os.Stderr.
func WithOutput(w io.Writer) Option {
	return func(o *options) { o.out = w }
}

// Collect gathers all available hardware identifiers.
// Errors from individual collectors are reported as warnings (when an
// output sink is configured via WithOutput) and never abort collection.
func Collect(opts ...Option) HardwareInfo {
	o := options{out: io.Discard}
	for _, opt := range opts {
		opt(&o)
	}

	hw := HardwareInfo{}

	if info, err := collectSystem(); err != nil {
		fmt.Fprintf(o.out, "warning: system info: %v\n", err)
	} else {
		hw.MachineID = info.MachineID
		hw.BootID = info.BootID
		hw.CPUInfo = info.CPUInfo
	}

	hw.DeviceTree = collectDeviceTree()
	hw.DMI = collectDMI()
	hw.TPM = collectTPM()
	hw.SecureBoot = collectSecureBoot()

	if ifaces, err := collectNetwork(); err != nil {
		fmt.Fprintf(o.out, "warning: network info: %v\n", err)
	} else {
		hw.NetworkInterfaces = ifaces
	}

	return hw
}
