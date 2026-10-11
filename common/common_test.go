package common

import (
	"strings"
	"testing"
)

func TestSafeFilename(t *testing.T) {
	got := SafeFilename(`../../ Very nice: extension!`)
	if got == "" || strings.ContainsAny(got, `/\\:`) || strings.Contains(got, "..") {
		t.Fatalf("unsafe filename: %q", got)
	}
	if SafeFilename("   ") != "store-item" {
		t.Fatal("empty names need a fallback")
	}
}
