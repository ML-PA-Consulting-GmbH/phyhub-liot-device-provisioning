package collector

import (
	"os"
	"strings"
)

// DMIInfo holds SMBIOS/DMI identifiers (UEFI systems only).
type DMIInfo struct {
	BoardSerial   string `json:"board_serial,omitempty"`
	BoardVendor   string `json:"board_vendor,omitempty"`
	BoardName     string `json:"board_name,omitempty"`
	ProductSerial string `json:"product_serial,omitempty"`
	ProductName   string `json:"product_name,omitempty"`
}

const dmiBase = "/sys/class/dmi/id/"

func collectDMI() DMIInfo {
	return DMIInfo{
		BoardSerial:   readDMI("board_serial"),
		BoardVendor:   readDMI("board_vendor"),
		BoardName:     readDMI("board_name"),
		ProductSerial: readDMI("product_serial"),
		ProductName:   readDMI("product_name"),
	}
}

func readDMI(field string) string {
	data, err := os.ReadFile(dmiBase + field)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}
