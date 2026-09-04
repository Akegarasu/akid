package metrics

import (
	"sync"

	"akid/internal/model"
)

type previousProcess struct {
	pid       int
	startTime uint64
	cpuTicks  uint64
}

// Sampler keeps only the previous counters needed to calculate a CPU delta.
// It does not own process state; PID identity always comes from ProcessInfo.
type Sampler struct {
	mu        sync.Mutex
	lastTotal uint64
	previous  map[string]previousProcess
}

func NewSampler() *Sampler {
	return &Sampler{previous: make(map[string]previousProcess)}
}

// Sample is implemented per platform. Unsupported platforms return metrics
// marked unavailable so protocol clients can render a placeholder.
var _ interface {
	Sample([]model.ProcessInfo) []model.ProcessMetrics
} = (*Sampler)(nil)
