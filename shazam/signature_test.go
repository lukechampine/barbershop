package shazam

import (
	"crypto/sha256"
	"fmt"
	"math"
	"testing"
)

func TestSignature(t *testing.T) {
	samples := make([]float64, 16*1024)
	for i := range samples {
		samples[i] = math.Sin(float64(i) * 2 * math.Pi / 256)
	}
	sig := ComputeSignature(16000, samples)
	h := sha256.Sum256(sig.encode())
	if fmt.Sprintf("%x", h) != "c8c055411ec845f6d57b27baf7fc5735fdaf51f2a6026dd12f09d0eb17652c02" {
		t.Fatalf("bad signature: %x", h)
	}
}
