//go:build !dev

package agent

import (
	"slices"
	"testing"
)

func TestOriginsRelease(t *testing.T) {
	if want := []string{"https://kts.zerdeli.com.kz:8443"}; !slices.Equal(Origins, want) {
		t.Fatalf("Origins = %q, ждали %q", Origins, want)
	}
}
