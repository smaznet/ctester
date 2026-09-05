package probe

import (
	"net/http"
	"testing"
)

func TestHeadersOK(t *testing.T) {
	h := http.Header{}
	h.Set("Location", "https://accounts.google.com/signin")
	h.Set("X-Foo", "bar-baz")

	if err := headersOK(h, nil); err != nil {
		t.Fatalf("nil expect: %v", err)
	}
	if err := headersOK(h, map[string]string{}); err != nil {
		t.Fatalf("empty expect: %v", err)
	}
	if err := headersOK(h, map[string]string{"Location": "accounts.google"}); err != nil {
		t.Fatalf("contains ok: %v", err)
	}
	if err := headersOK(h, map[string]string{"location": "accounts.google"}); err != nil {
		t.Fatalf("case-insensitive name: %v", err)
	}
	if err := headersOK(h, map[string]string{"Location": "nowhere"}); err == nil {
		t.Fatal("expected mismatch")
	}
	if err := headersOK(h, map[string]string{"Missing": "x"}); err == nil {
		t.Fatal("expected missing")
	}
	if err := headersOK(h, map[string]string{"X-Foo": "bar", "Location": "google"}); err != nil {
		t.Fatalf("multi ok: %v", err)
	}
}
