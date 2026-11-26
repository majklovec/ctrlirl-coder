package balancers

import (
	"testing"

	"ctrlirl-coder/internal/sinks"
)

func TestAIMDAdditiveIncrease(t *testing.T) {
	p := Params{
		MinBitrate:       500_000,
		MaxBitrate:       6_000_000,
		AimdIncrStep:     50_000,
		AimdDecrMult:     0.75,
		AimdIncrInterval: 500,
		AimdDecrInterval: 200,
	}
	a := NewAIMD(p)

	if got := a.Next(2_000_000, sinks.Stats{TimestampMs: 100}, p); got != 2_050_000 {
		t.Fatalf("first increase = %d, want 2050000", got)
	}
	if got := a.Next(2_050_000, sinks.Stats{TimestampMs: 599}, p); got != 2_050_000 {
		t.Fatalf("increase before interval = %d, want 2050000", got)
	}
	if got := a.Next(2_050_000, sinks.Stats{TimestampMs: 600}, p); got != 2_100_000 {
		t.Fatalf("increase at interval = %d, want 2100000", got)
	}
}

func TestAIMDMultiplicativeDecrease(t *testing.T) {
	p := Params{
		MinBitrate:       500_000,
		MaxBitrate:       6_000_000,
		AimdIncrStep:     50_000,
		AimdDecrMult:     0.75,
		AimdIncrInterval: 500,
		AimdDecrInterval: 200,
	}
	a := NewAIMD(p)

	got := a.Next(2_000_000, sinks.Stats{
		TimestampMs:     100,
		PktSndLossTotal: 1,
		PktRetransTotal: 1,
		PacketLossRate:  1,
	}, p)
	if got != 1_500_000 {
		t.Fatalf("loss decrease = %d, want 1500000", got)
	}

	if got = a.Next(1_500_000, sinks.Stats{
		TimestampMs:     299,
		PktSndLossTotal: 1,
		PktRetransTotal: 1,
		PacketLossRate:  0,
	}, p); got != 1_500_000 {
		t.Fatalf("decrease before interval = %d, want 1500000", got)
	}
}

func TestAIMDClampsToConfiguredBounds(t *testing.T) {
	p := Params{
		MinBitrate:       500_000,
		MaxBitrate:       6_000_000,
		AimdIncrStep:     50_000,
		AimdDecrMult:     0.75,
		AimdIncrInterval: 500,
		AimdDecrInterval: 200,
	}
	a := NewAIMD(p)

	if got := a.Next(5_990_000, sinks.Stats{TimestampMs: 100}, p); got != 6_000_000 {
		t.Fatalf("upper clamp = %d, want 6000000", got)
	}
	if got := a.Next(500_000, sinks.Stats{
		TimestampMs:     200,
		PktSndLossTotal: 1,
		PacketLossRate:  1,
	}, p); got != 500_000 {
		t.Fatalf("lower clamp = %d, want 500000", got)
	}
}
