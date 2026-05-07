package collector

import (
	"os"
	"path/filepath"
)

// SecureBootInfo describes the Secure Boot state (UEFI systems only).
type SecureBootInfo struct {
	Available bool `json:"available"`
	Enabled   bool `json:"enabled"`
}

const efiVarsDir = "/sys/firmware/efi/efivars/"

// secureBootEnabled is the value byte indicating Secure Boot is active.
// The EFI SecureBoot variable layout is 4 attribute bytes followed by a
// single-byte value: 0x00 = disabled, 0x01 = enabled.
const secureBootEnabled byte = 0x01

func collectSecureBoot() SecureBootInfo {
	if _, err := os.Stat(efiVarsDir); err != nil {
		// No EFI; common on Yocto/ARM boards without UEFI
		return SecureBootInfo{Available: false, Enabled: false}
	}

	matches, err := filepath.Glob(efiVarsDir + "SecureBoot-*")
	if err != nil || len(matches) == 0 {
		return SecureBootInfo{Available: true, Enabled: false}
	}

	data, err := os.ReadFile(matches[0])
	// The variable is 4 attribute bytes followed by the 1-byte value, so a
	// well-formed entry is exactly 5 bytes. Guard against a short/malformed
	// read rather than indexing blindly.
	if err != nil || len(data) < 5 {
		return SecureBootInfo{Available: true, Enabled: false}
	}

	enabled := data[len(data)-1] == secureBootEnabled
	return SecureBootInfo{Available: true, Enabled: enabled}
}
