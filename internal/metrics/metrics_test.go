package metrics

import (
	"math"
	"testing"
)

func TestCompute(t *testing.T) {
	m, ok := Compute([]int{0, 1, 2}, []float64{100, 110, 99})
	if !ok {
		t.Fatal("expected metrics")
	}
	if math.Abs(m.TotalReturn-(-0.01)) > 1e-12 {
		t.Fatalf("total return %v", m.TotalReturn)
	}
	if math.Abs(m.MaxDrawdown-(99.0/110-1)) > 1e-12 {
		t.Fatalf("max drawdown %v", m.MaxDrawdown)
	}
	if _, ok := Compute([]int{0}, []float64{100}); ok {
		t.Fatal("one point is not a series")
	}
}
