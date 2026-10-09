package main

import (
	"strings"
	"testing"
)

func TestSafeFilename(t *testing.T) {
	got := safeFilename(`../../ Very nice: extension!`)
	if got == "" || strings.ContainsAny(got, `/\\:`) || strings.Contains(got, "..") {
		t.Fatalf("unsafe filename: %q", got)
	}
}
