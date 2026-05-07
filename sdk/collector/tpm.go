package collector

import (
	"os"
	"strings"
)

// TPMInfo describes the TPM, if present.
type TPMInfo struct {
	Present      bool   `json:"present"`
	Version      string `json:"version,omitempty"`
	Type         string `json:"type,omitempty"` // "hardware", "optee", "firmware"
	Modalias     string `json:"modalias,omitempty"`
	DevicePath   string `json:"device_path,omitempty"`
}

// knownHWPrefixes are ACPI/modalias prefixes for discrete hardware TPM chips.
var knownHWPrefixes = []string{
	"acpi:STM",  // STMicroelectronics
	"acpi:NTC",  // Nuvoton
	"acpi:IFX",  // Infineon
	"acpi:BCM",  // Broadcom
	"acpi:INTC", // Intel PTT (platform, but discrete-backed on most boards)
}

func collectTPM() TPMInfo {
	// Find first available TPM device node
	devicePath := ""
	for _, p := range []string{"/dev/tpmrm0", "/dev/tpm0"} {
		if _, err := os.Stat(p); err == nil {
			devicePath = p
			break
		}
	}

	if devicePath == "" {
		return TPMInfo{Present: false}
	}

	info := TPMInfo{
		Present:    true,
		DevicePath: devicePath,
	}

	const sysTPM = "/sys/class/tpm/tpm0/"

	if data, err := os.ReadFile(sysTPM + "tpm_version_major"); err == nil {
		info.Version = strings.TrimSpace(string(data))
	}

	modalias := ""
	if data, err := os.ReadFile(sysTPM + "device/modalias"); err == nil {
		modalias = strings.TrimSpace(string(data))
		info.Modalias = modalias
	}

	info.Type = classifyTPM(modalias, sysTPM)
	return info
}

func classifyTPM(modalias, sysTPMPath string) string {
	lower := strings.ToLower(modalias)

	// OP-TEE firmware TPM: device lives under the optee bus
	if strings.Contains(lower, "optee") {
		return "optee"
	}

	// Check if the device path resolves through an optee platform device
	if target, err := os.Readlink(sysTPMPath + "device"); err == nil {
		if strings.Contains(strings.ToLower(target), "optee") {
			return "optee"
		}
	}

	for _, prefix := range knownHWPrefixes {
		if strings.HasPrefix(modalias, prefix) {
			return "hardware"
		}
	}

	// Microsoft placeholder (Hyper-V vTPM, firmware TPM on some ARM boards)
	if strings.Contains(modalias, "MSFT0101") {
		return "firmware"
	}

	return "firmware"
}
