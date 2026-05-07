// Package payload maps locally-collected device facts into the JSON shapes
// snapd expects in the L-IoT registration POST body.
//
// Snapd-owned fields (format_version, nonce, snap.assertions_b64,
// attestation.tpm.*) are intentionally not produced here; snapd injects
// them at assembly time.
package payload

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"phyhub-liot-device-provisioning/sdk/collector"
)

// CollectorIdentity is the {"name", "version", "binary_sha256"} object that
// goes into the registration body's collector field. binary_sha256 is
// optional and identifies the exact binary that produced the payload; see
// SelfBinarySHA256.
type CollectorIdentity struct {
	Name         string `json:"name"`
	Version      string `json:"version"`
	BinarySHA256 string `json:"binary_sha256,omitempty"`
}

// CollectorJSON marshals a CollectorIdentity into the JSON shape snapd
// stores under "collector". Pass an empty binarySHA256 to omit the field.
func CollectorJSON(name, version, binarySHA256 string) (json.RawMessage, error) {
	return json.Marshal(CollectorIdentity{
		Name:         name,
		Version:      version,
		BinarySHA256: binarySHA256,
	})
}

// SelfBinarySHA256 returns the lowercase hex SHA-256 of the currently
// running executable. Best-effort: returns ("", err) if os.Executable or
// the read fails; the caller should treat that as "omit the field".
func SelfBinarySHA256() (string, error) {
	path, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("locate self: %w", err)
	}
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open self %q: %w", path, err)
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("hash self %q: %w", path, err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// ClaimJSON marshals a claiming token into the JSON shape snapd stores
// under "claim". An empty token returns nil (omit-the-field semantics).
func ClaimJSON(token string) (json.RawMessage, error) {
	if token == "" {
		return nil, nil
	}
	return json.Marshal(struct {
		Token string `json:"token"`
	}{Token: token})
}

// HardwareJSON converts a collector.HardwareInfo into the spec-shaped
// "hardware" object. The spec uses a flatter layout than the collector's
// internal types: dmi/device_tree/cpu live under "platform"; tpm describes
// the chip (presence, version, type, device path); secure_boot has
// {available, enabled}; network_interfaces are name+mac+optional type.
//
// All fields are best-effort: zero values are omitted via omitempty.
func HardwareJSON(hw collector.HardwareInfo) (json.RawMessage, error) {
	out := hardwareDoc{
		MachineID:         hw.MachineID,
		Hostname:          readHostname(),
		Platform:          mapPlatform(hw),
		TPM:               mapTPM(hw.TPM),
		SecureBoot:        secureBootDoc{Available: hw.SecureBoot.Available, Enabled: hw.SecureBoot.Enabled},
		NetworkInterfaces: mapNetwork(hw.NetworkInterfaces),
	}
	return json.Marshal(out)
}

// SoftwareJSON returns the snapd-spec-shaped "software" object. For now
// nothing is reported by the provisioning tool: snapd already knows
// authoritatively which apps and image are installed, and querying that
// inventory from the tool side would just duplicate state. Returns nil so
// the caller omits the field entirely.
func SoftwareJSON() (json.RawMessage, error) {
	return nil, nil
}

// --- Internal types matching the spec wire shape ----------------------------

// hardwareDoc mirrors the spec's "hardware" wire shape. Optional inner
// objects are pointers so a nil value drops via `omitempty`; Go's
// `omitempty` does NOT drop zero-valued struct values, only nil pointers
// (and empty primitives/slices/maps), so the optional sub-objects must
// be addressable. tpm and secure_boot are intentionally non-pointer:
// their boolean fields are meaningful at false ("we checked, absent").
type hardwareDoc struct {
	MachineID         string            `json:"machine_id,omitempty"`
	Hostname          string            `json:"hostname,omitempty"`
	Platform          *platformDoc      `json:"platform,omitempty"`
	TPM               tpmChipDoc        `json:"tpm"`
	SecureBoot        secureBootDoc     `json:"secure_boot"`
	NetworkInterfaces []networkIfaceDoc `json:"network_interfaces,omitempty"`
}

type platformDoc struct {
	DMI        *dmiDoc        `json:"dmi,omitempty"`
	DeviceTree *deviceTreeDoc `json:"device_tree,omitempty"`
	CPU        *cpuDoc        `json:"cpu,omitempty"`
}

type dmiDoc struct {
	BoardVendor string `json:"board_vendor,omitempty"`
	BoardName   string `json:"board_name,omitempty"`
	ProductName string `json:"product_name,omitempty"`
}

type deviceTreeDoc struct {
	Model      string `json:"model,omitempty"`
	Compatible string `json:"compatible,omitempty"`
}

type cpuDoc struct {
	Model string `json:"model,omitempty"`
}

// tpmChipDoc is always emitted: Present=false carries the meaningful
// fact "the device has no TPM that we could detect".
type tpmChipDoc struct {
	Present    bool   `json:"present"`
	Version    string `json:"version,omitempty"`
	Type       string `json:"type,omitempty"`
	Modalias   string `json:"modalias,omitempty"`
	DevicePath string `json:"device_path,omitempty"`
}

type secureBootDoc struct {
	Available bool `json:"available"`
	Enabled   bool `json:"enabled"`
}

type networkIfaceDoc struct {
	Name string `json:"name"`
	MAC  string `json:"mac"`
	Type string `json:"type,omitempty"`
}

// --- Mappers ----------------------------------------------------------------

// mapPlatform builds the platform sub-object, returning nil when every
// child is empty so the parent's `omitempty` drops the whole field.
func mapPlatform(hw collector.HardwareInfo) *platformDoc {
	dmi := mapDMI(hw.DMI)
	dt := mapDeviceTree(hw.DeviceTree)
	cpu := mapCPU(hw.CPUInfo)
	if dmi == nil && dt == nil && cpu == nil {
		return nil
	}
	return &platformDoc{DMI: dmi, DeviceTree: dt, CPU: cpu}
}

func mapDMI(in collector.DMIInfo) *dmiDoc {
	if in.BoardVendor == "" && in.BoardName == "" && in.ProductName == "" {
		return nil
	}
	return &dmiDoc{
		BoardVendor: in.BoardVendor,
		BoardName:   in.BoardName,
		ProductName: in.ProductName,
	}
}

func mapDeviceTree(in collector.DeviceTreeInfo) *deviceTreeDoc {
	// The spec's "compatible" is a single string (the root compatible).
	// The collector returns the null-separated list; the first entry is
	// the most specific, which is what the spec wants.
	first := ""
	if len(in.Compatible) > 0 {
		first = in.Compatible[0]
	}
	// Model is not currently captured by the collector; device-tree
	// exposes /proc/device-tree/model; adding that to the collector is
	// a follow-up. Until then "compatible" is the only field that may
	// be set, and an empty one means we have nothing to report.
	if first == "" {
		return nil
	}
	return &deviceTreeDoc{Compatible: first}
}

func mapCPU(in collector.CPUInfo) *cpuDoc {
	if in.Model == "" {
		return nil
	}
	return &cpuDoc{Model: in.Model}
}

func mapTPM(in collector.TPMInfo) tpmChipDoc {
	return tpmChipDoc{
		Present:    in.Present,
		Version:    in.Version,
		Type:       in.Type,
		Modalias:   in.Modalias,
		DevicePath: in.DevicePath,
	}
}

func mapNetwork(in []collector.NetworkInterface) []networkIfaceDoc {
	if len(in) == 0 {
		return nil
	}
	out := make([]networkIfaceDoc, 0, len(in))
	for _, n := range in {
		out = append(out, networkIfaceDoc{Name: n.Name, MAC: n.MAC})
	}
	return out
}

func readHostname() string {
	if data, err := os.ReadFile("/etc/hostname"); err == nil {
		return strings.TrimSpace(string(data))
	}
	if h, err := os.Hostname(); err == nil {
		return h
	}
	return ""
}
