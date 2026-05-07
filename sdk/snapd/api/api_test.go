package api

import "testing"

const sampleSerialAssertion = `type: serial
authority-id: generic
brand-id: generic
model: generic-classic
serial: 46923e6d-5d45-420d-905a-99a9e92493b4
device-key:
  AcbBTQRWhcGAARAA1234
  ...AEQEAAQ==
device-key-sha3-384: PznqOqWAx4_f8tFafGI2
timestamp: 2025-07-17T03:11:33.518427Z
sign-key-sha3-384: wrfougkz3Huq2T_Kklfnu

AcLBcwQAAQoAHQUC...
...bX5JkJG5cunW0h/
`

func TestParseAssertionHeaders_SingleLineFields(t *testing.T) {
	hdrs, err := parseAssertionHeaders(sampleSerialAssertion)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	cases := map[string]string{
		"type":                "serial",
		"authority-id":        "generic",
		"brand-id":            "generic",
		"model":               "generic-classic",
		"serial":              "46923e6d-5d45-420d-905a-99a9e92493b4",
		"device-key-sha3-384": "PznqOqWAx4_f8tFafGI2",
		"timestamp":           "2025-07-17T03:11:33.518427Z",
		"sign-key-sha3-384":   "wrfougkz3Huq2T_Kklfnu",
	}
	for k, want := range cases {
		if got := hdrs[k]; got != want {
			t.Errorf("header %q: got %q, want %q", k, got, want)
		}
	}
}

func TestParseAssertionHeaders_StopsAtBlankLine(t *testing.T) {
	hdrs, err := parseAssertionHeaders(sampleSerialAssertion)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Anything after the empty separator line (signature body) must NOT
	// have leaked into headers.
	if _, ok := hdrs["AcLBcwQAAQoAHQUC"]; ok {
		t.Errorf("signature body bled into headers")
	}
}

func TestSerialAssertion_Accessors(t *testing.T) {
	a := &SerialAssertion{
		Headers: map[string]string{
			"serial":   "abc-123",
			"brand-id": "canonical",
			"model":    "ubuntu-core-24-amd64",
		},
	}
	if a.Serial() != "abc-123" {
		t.Errorf("Serial(): got %q", a.Serial())
	}
	if a.BrandID() != "canonical" {
		t.Errorf("BrandID(): got %q", a.BrandID())
	}
	if a.Model() != "ubuntu-core-24-amd64" {
		t.Errorf("Model(): got %q", a.Model())
	}
}
