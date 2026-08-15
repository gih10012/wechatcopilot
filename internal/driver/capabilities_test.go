package driver

import "testing"

func TestCapabilityMapIsCompleteAndRejectsForkedKeys(t *testing.T) {
	values := CapabilityMap(map[string]Support{CapabilityMessagesSend: SupportBeta})
	if err := ValidateCapabilities(values); err != nil {
		t.Fatal(err)
	}
	if values[CapabilityMessagesSend] != SupportBeta || values[CapabilityAuthQR] != SupportUnsupported {
		t.Fatalf("unexpected complete capability map: %#v", values)
	}

	defer func() {
		if recover() == nil {
			t.Fatal("CapabilityMap accepted an unknown public key")
		}
	}()
	CapabilityMap(map[string]Support{"send.text": SupportStable})
}
