package client

import (
	"strings"
	"testing"
)

func TestDecodeDaemonResponseEnforcesWholeBodyLimit(t *testing.T) {
	base := `{"schema_version":"1","ok":true,"data":{"value":"ok"}}`
	exactMaximum := int64(len(base) + 4)
	envelope, err := decodeDaemonResponse(strings.NewReader(base+"    "), exactMaximum)
	if err != nil {
		t.Fatalf("decode response at exact limit: %v", err)
	}
	if !envelope.OK || string(envelope.Data) != `{"value":"ok"}` {
		t.Fatalf("decoded envelope = %+v", envelope)
	}

	_, err = decodeDaemonResponse(strings.NewReader(base+"     "), exactMaximum)
	if err == nil || !strings.Contains(err.Error(), "response exceeds") {
		t.Fatalf("oversized response error = %v, want explicit size error", err)
	}
}

func TestDecodeDaemonResponseRejectsTrailingJSON(t *testing.T) {
	input := `{"schema_version":"1","ok":true} {"unexpected":true}`
	_, err := decodeDaemonResponse(strings.NewReader(input), int64(len(input)+1))
	if err == nil || !strings.Contains(err.Error(), "trailing data") {
		t.Fatalf("trailing response error = %v, want trailing data error", err)
	}
}

func TestDecodeDaemonResponseRejectsInvalidLimit(t *testing.T) {
	if _, err := decodeDaemonResponse(strings.NewReader(`{}`), 0); err == nil {
		t.Fatal("zero response limit was accepted")
	}
}
