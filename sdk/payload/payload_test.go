package payload

import (
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"

	"phyhub-liot-device-provisioning/sdk/collector"
)

// decode unmarshals raw JSON into a free-form map for shape assertions.
// Using map[string]any lets the tests check both presence and value
// without committing to a Go struct that mirrors the spec; if the spec
// changes, the failure points at the actual wire shape.
func decode(t *testing.T, raw json.RawMessage) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return out
}

// --- ClaimJSON -----------------------------------------------------------

func TestClaimJSON_EmptyTokenReturnsNil(t *testing.T) {
	// Spec semantics: empty token must produce nil so the caller's
	// `omitempty` drops the whole "claim" field. A wrapper object like
	// `{"token":""}` would be a regression: snapd treats an empty token
	// as a present-but-bad claim and would reject the payload.
	raw, err := ClaimJSON("")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if raw != nil {
		t.Errorf("expected nil RawMessage, got %s", raw)
	}
}

func TestClaimJSON_TokenIsWrapped(t *testing.T) {
	raw, err := ClaimJSON("ABCD-1234-EFGH")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := decode(t, raw)
	if got["token"] != "ABCD-1234-EFGH" {
		t.Errorf("token field: got %v", got["token"])
	}
	// Wire shape must be exactly {"token": "..."}; extra fields would
	// be visible on the snapd→Appstore POST.
	if len(got) != 1 {
		t.Errorf("unexpected extra fields: %v", got)
	}
}

// --- CollectorJSON -------------------------------------------------------

func TestCollectorJSON_OmitsEmptyBinarySHA(t *testing.T) {
	raw, err := CollectorJSON("liot-provisioning", "1.2.3", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := decode(t, raw)
	if got["name"] != "liot-provisioning" || got["version"] != "1.2.3" {
		t.Errorf("name/version wrong: %v", got)
	}
	if _, has := got["binary_sha256"]; has {
		t.Errorf("empty binary_sha256 must be omitted, got %v", got)
	}
}

func TestCollectorJSON_IncludesBinarySHA(t *testing.T) {
	raw, err := CollectorJSON("liot-provisioning", "1.2.3", "deadbeef")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := decode(t, raw)
	if got["binary_sha256"] != "deadbeef" {
		t.Errorf("binary_sha256: got %v", got["binary_sha256"])
	}
}

// --- SelfBinarySHA256 ---------------------------------------------------

func TestSelfBinarySHA256_ReturnsLowercaseHex(t *testing.T) {
	// Best-effort by design, but in `go test` the test binary is on
	// disk, so this must succeed. If it ever doesn't, the production
	// "omit-the-field" fallback would silently kick in, hiding bugs.
	got, err := SelfBinarySHA256()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 64 {
		t.Errorf("sha256 hex: got %d chars, want 64", len(got))
	}
	if _, derr := hex.DecodeString(got); derr != nil {
		t.Errorf("not valid hex: %q (%v)", got, derr)
	}
	if got != strings.ToLower(got) {
		t.Errorf("expected lowercase hex, got %q", got)
	}
}

// --- SoftwareJSON --------------------------------------------------------

func TestSoftwareJSON_ReturnsNil(t *testing.T) {
	// Currently a placeholder: we explicitly send no software inventory
	// because snapd already has the authoritative view. The contract
	// (returns nil so the caller omits the field) is what we test.
	raw, err := SoftwareJSON()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if raw != nil {
		t.Errorf("expected nil, got %s", raw)
	}
}

// --- HardwareJSON --------------------------------------------------------

func TestHardwareJSON_FullPayloadShape(t *testing.T) {
	hw := collector.HardwareInfo{
		MachineID: "machine-id-abc",
		BootID:    "boot-id-xyz", // intentionally not in spec → must be absent
		CPUInfo:   collector.CPUInfo{Model: "Cortex-A72"},
		DeviceTree: collector.DeviceTreeInfo{
			Compatible:   []string{"raspberrypi,4-model-b", "brcm,bcm2711"},
			SerialNumber: "deadbeef",
		},
		DMI: collector.DMIInfo{
			BoardVendor: "ACME",
			BoardName:   "Foo",
			ProductName: "Bar",
		},
		TPM: collector.TPMInfo{
			Present:    true,
			Version:    "2.0",
			Type:       "hardware",
			DevicePath: "/dev/tpm0",
		},
		SecureBoot: collector.SecureBootInfo{Available: true, Enabled: true},
		NetworkInterfaces: []collector.NetworkInterface{
			{Name: "eth0", MAC: "aa:bb:cc:dd:ee:ff"},
			{Name: "wlan0", MAC: "11:22:33:44:55:66"},
		},
	}

	raw, err := HardwareJSON(hw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := decode(t, raw)

	if got["machine_id"] != "machine-id-abc" {
		t.Errorf("machine_id: got %v", got["machine_id"])
	}
	// boot_id is part of the collector struct but not part of the spec;
	// HardwareJSON must drop it to avoid leaking unrelated identifiers.
	if _, has := got["boot_id"]; has {
		t.Errorf("boot_id leaked into payload: %v", got)
	}

	platform, ok := got["platform"].(map[string]any)
	if !ok {
		t.Fatalf("platform missing or wrong type: %T", got["platform"])
	}

	dmi, _ := platform["dmi"].(map[string]any)
	if dmi["board_vendor"] != "ACME" || dmi["product_name"] != "Bar" {
		t.Errorf("dmi mapped incorrectly: %v", dmi)
	}

	dt, _ := platform["device_tree"].(map[string]any)
	// Compatible is a single string in the spec, taken from the *first*
	// (most-specific) entry of the collector's slice.
	if dt["compatible"] != "raspberrypi,4-model-b" {
		t.Errorf("device_tree.compatible: got %v, want most-specific entry", dt["compatible"])
	}

	cpu, _ := platform["cpu"].(map[string]any)
	if cpu["model"] != "Cortex-A72" {
		t.Errorf("cpu.model: got %v", cpu["model"])
	}

	tpm, _ := got["tpm"].(map[string]any)
	if tpm["present"] != true || tpm["version"] != "2.0" || tpm["type"] != "hardware" {
		t.Errorf("tpm mapped incorrectly: %v", tpm)
	}

	sb, _ := got["secure_boot"].(map[string]any)
	if sb["available"] != true || sb["enabled"] != true {
		t.Errorf("secure_boot mapped incorrectly: %v", sb)
	}

	ifaces, _ := got["network_interfaces"].([]any)
	if len(ifaces) != 2 {
		t.Fatalf("expected 2 network interfaces, got %d", len(ifaces))
	}
	first, _ := ifaces[0].(map[string]any)
	if first["name"] != "eth0" || first["mac"] != "aa:bb:cc:dd:ee:ff" {
		t.Errorf("first iface: %v", first)
	}
}

func TestHardwareJSON_OmitsEmptyOptionalFields(t *testing.T) {
	// A bare HardwareInfo (everything zero) must produce a JSON object
	// where every truly-optional field is gone: no empty `platform: {}`
	// or `dmi: {}` waste. tpm and secure_boot are intentionally retained
	// because their boolean fields carry meaning at false ("we checked,
	// it's not there").
	raw, err := HardwareJSON(collector.HardwareInfo{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := decode(t, raw)

	for _, omitted := range []string{"machine_id", "platform", "network_interfaces"} {
		if _, has := got[omitted]; has {
			t.Errorf("expected %q to be omitted on zero value, payload=%v", omitted, got)
		}
	}

	tpm, ok := got["tpm"].(map[string]any)
	if !ok {
		t.Fatalf("tpm should be present even when empty, got %v", got)
	}
	if tpm["present"] != false {
		t.Errorf("tpm.present must be present and false: %v", tpm)
	}

	sb, ok := got["secure_boot"].(map[string]any)
	if !ok {
		t.Fatalf("secure_boot should be present even when empty, got %v", got)
	}
	if sb["available"] != false || sb["enabled"] != false {
		t.Errorf("secure_boot booleans must be present: %v", sb)
	}
}

func TestHardwareJSON_PartialPlatformOnlyEmitsPopulated(t *testing.T) {
	// Only CPU populated → platform present, but with cpu only. dmi and
	// device_tree must drop, not appear as empty objects.
	raw, err := HardwareJSON(collector.HardwareInfo{
		CPUInfo: collector.CPUInfo{Model: "Cortex-A72"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := decode(t, raw)
	platform, ok := got["platform"].(map[string]any)
	if !ok {
		t.Fatalf("platform should be present when CPU populated: %v", got)
	}
	if cpu, _ := platform["cpu"].(map[string]any); cpu["model"] != "Cortex-A72" {
		t.Errorf("cpu.model wrong: %v", platform)
	}
	for _, omitted := range []string{"dmi", "device_tree"} {
		if _, has := platform[omitted]; has {
			t.Errorf("%q should drop when empty, got %v", omitted, platform)
		}
	}
}

func TestHardwareJSON_HostnameIsPopulated(t *testing.T) {
	// readHostname falls back to os.Hostname when /etc/hostname is
	// unreadable, so in a normal test env we always get a non-empty
	// string. If this ever empties out, every device payload would be
	// missing the hostname, which is worth catching.
	raw, err := HardwareJSON(collector.HardwareInfo{MachineID: "mid"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := decode(t, raw)
	if got["hostname"] == nil || got["hostname"] == "" {
		t.Errorf("hostname missing from payload: %v", got)
	}
}

func TestHardwareJSON_DeviceTreeEmptyCompatible(t *testing.T) {
	// Empty compatible slice means "nothing to report" → device_tree
	// drops entirely. Combined with no DMI / CPU, platform itself
	// drops as well.
	raw, err := HardwareJSON(collector.HardwareInfo{
		DeviceTree: collector.DeviceTreeInfo{Compatible: nil},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := decode(t, raw)
	if _, has := got["platform"]; has {
		t.Errorf("platform should drop when all sub-objects empty, got %v", got)
	}
}
