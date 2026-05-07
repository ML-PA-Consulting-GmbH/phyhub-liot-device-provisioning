package collector

import (
	"encoding/hex"
	"os"
	"path/filepath"
)

// EEPROMInfo holds raw EEPROM data from an I2C device.
type EEPROMInfo struct {
	Path string `json:"path"`
	Hex  string `json:"hex"`
}

const i2cDevDir = "/sys/bus/i2c/devices/"

// collectEEPROM finds the first readable EEPROM on the I2C bus.
// Returns nil if none is found (non-error absence).
func collectEEPROM() (*EEPROMInfo, error) {
	matches, err := filepath.Glob(i2cDevDir + "*/eeprom")
	if err != nil {
		return nil, err
	}
	if len(matches) == 0 {
		return nil, nil
	}

	for _, path := range matches {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		if len(data) == 0 {
			continue
		}

		// Cap at 256 bytes to keep the JSON payload reasonable
		if len(data) > 256 {
			data = data[:256]
		}

		return &EEPROMInfo{
			Path: path,
			Hex:  hex.EncodeToString(data),
		}, nil
	}

	return nil, nil
}
