package config

import (
	"strings"
	"testing"
)

func TestBannerPutsWordmarkOnEyeRow(t *testing.T) {
	lines := strings.Split(strings.Trim(banner(), "\n"), "\n")
	if len(lines) != len(logoLines) {
		t.Fatalf("banner has %d lines, want %d", len(lines), len(logoLines))
	}
	for i, l := range lines {
		has := strings.Contains(l, "var") && strings.Contains(l, "top")
		if has != (i == 3) {
			t.Errorf("line %d: wordmark present = %v, want %v: %q", i, has, i == 3, l)
		}
	}
}
