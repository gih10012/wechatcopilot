package api

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDecodeJSONRequiresOneBoundedValue(t *testing.T) {
	tests := []struct {
		name string
		body string
		ok   bool
	}{
		{name: "one value", body: `{"name":"fixture"}`, ok: true},
		{name: "second value", body: `{"name":"fixture"} {"name":"other"}`},
		{name: "unknown field", body: `{"name":"fixture","extra":true}`},
		{name: "oversized trailing whitespace", body: `{"name":"fixture"}` + strings.Repeat(" ", (2<<20)+1)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest("POST", "/", strings.NewReader(test.body))
			var value struct {
				Name string `json:"name"`
			}
			err := DecodeJSON(request, &value)
			if test.ok && err != nil {
				t.Fatalf("DecodeJSON rejected one bounded value: %v", err)
			}
			if !test.ok && err == nil {
				t.Fatal("DecodeJSON accepted an invalid body")
			}
		})
	}
}
