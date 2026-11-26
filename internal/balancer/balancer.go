// Package balancer exposes the bitrate balancer API and factory.
package balancer

import (
	"ctrlirl-coder/internal/balancer/balancers"
	"ctrlirl-coder/internal/sinks"
)

// Balancer computes the next target video bitrate (in bits per second) from
// the current target, a fresh SRT statistics sample, and the tuning
// parameters.
//
// Implementations may be stateful (adaptive and aimd keep moving averages and
// rate-limit timers), in which case they must be owned by a single caller
// (the controller's balancer ticker) and are not goroutine-safe.
type Balancer interface {
	// Name returns the algorithm name ("fixed", "adaptive", "aimd").
	Name() string

	// Next returns the new target bitrate in bps.
	Next(currentBPS int, s sinks.Stats, p Params) int
}

// Params carries every tunable that an algorithm needs.
type Params = balancers.Params

// Concrete implementation types are also available from the balancer package
// for callers that need direct construction.
type Adaptive = balancers.Adaptive
type AIMD = balancers.AIMD
type Fixed = balancers.Fixed

// Algorithms lists the accepted algorithm names, for config validation and
// --help output.
var Algorithms = []string{"adaptive", "fixed", "aimd"}

// New builds a balancer for the named algorithm.
func New(name string, p Params) Balancer {
	switch name {
	case "fixed":
		return NewFixed()
	case "aimd":
		return NewAIMD(p)
	case "adaptive", "":
		return NewAdaptive(p)
	default:
		return NewAdaptive(p)
	}
}

func NewFixed() *Fixed {
	return balancers.NewFixed()
}

func NewAdaptive(p Params) *Adaptive {
	return balancers.NewAdaptive(p)
}

func NewAIMD(p Params) *AIMD {
	return balancers.NewAIMD(p)
}
