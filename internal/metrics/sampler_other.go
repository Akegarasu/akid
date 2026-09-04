//go:build !linux

package metrics

import "akid/internal/model"

func (s *Sampler) Sample(processes []model.ProcessInfo) []model.ProcessMetrics {
	metrics := make([]model.ProcessMetrics, 0, len(processes))
	for _, info := range processes {
		metrics = append(metrics, model.ProcessMetrics{ID: info.Config.ID, PID: info.Runtime.PID})
	}
	return metrics
}
