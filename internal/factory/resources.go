package factory

import (
	"fmt"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

type ResourceManager struct {
	mu        sync.RWMutex
	allocated map[string]*AllocatedResources
	available SystemResources
	limits    SystemLimits
}

type SystemLimits struct {
	MaxMemory int64
	MaxCPU    float64
	MaxAgents int
}

func NewResourceManager() *ResourceManager {
	return &ResourceManager{
		allocated: make(map[string]*AllocatedResources),
		available: SystemResources{
			TotalMemory:     getSystemMemory(),
			TotalCPU:        float64(runtime.NumCPU()),
			AvailableMemory: getSystemMemory(),
			AvailableCPU:    float64(runtime.NumCPU()),
		},
		limits: SystemLimits{
			MaxMemory: getSystemMemory(),
			MaxCPU:    float64(runtime.NumCPU()),
			MaxAgents: 20,
		},
	}
}

func (rm *ResourceManager) CanAllocate(limits ResourceLimits) error {
	rm.mu.RLock()
	defer rm.mu.RUnlock()

	memoryBytes := parseMemory(limits.Memory)
	if memoryBytes > rm.available.AvailableMemory {
		return fmt.Errorf("insufficient memory: need %s, available %s",
			limits.Memory, formatBytes(rm.available.AvailableMemory))
	}

	if float64(limits.CPU) > rm.available.AvailableCPU {
		return fmt.Errorf("insufficient CPU: need %d, available %.1f",
			limits.CPU, rm.available.AvailableCPU)
	}

	if len(rm.allocated) >= rm.limits.MaxAgents {
		return fmt.Errorf("maximum number of agents (%d) reached", rm.limits.MaxAgents)
	}

	return nil
}

func (rm *ResourceManager) Allocate(agent *Agent, limits ResourceLimits) error {
	rm.mu.Lock()
	defer rm.mu.Unlock()

	memoryBytes := parseMemory(limits.Memory)

	allocation := &AllocatedResources{
		AgentID:   agent.ID,
		Memory:    memoryBytes,
		CPU:       float64(limits.CPU),
		APITokens: make(map[string]int),
		StartTime: time.Now(),
	}

	rm.available.AvailableMemory -= memoryBytes
	rm.available.AvailableCPU -= float64(limits.CPU)

	rm.allocated[agent.ID] = allocation
	agent.Resources = allocation

	return nil
}

func (rm *ResourceManager) Release(agentID string) error {
	rm.mu.Lock()
	defer rm.mu.Unlock()

	allocation, ok := rm.allocated[agentID]
	if !ok {
		return fmt.Errorf("no resources allocated for agent %s", agentID)
	}

	rm.available.AvailableMemory += allocation.Memory
	rm.available.AvailableCPU += allocation.CPU

	delete(rm.allocated, agentID)
	return nil
}

func (rm *ResourceManager) GetUsageReport() *ResourceReport {
	rm.mu.RLock()
	defer rm.mu.RUnlock()

	report := &ResourceReport{
		Timestamp: time.Now(),
		System:    rm.available,
		Agents:    make([]AgentResourceUsage, 0, len(rm.allocated)),
	}

	for agentID, alloc := range rm.allocated {
		usage := AgentResourceUsage{
			AgentID:  agentID,
			Memory:   alloc.Memory,
			CPU:      alloc.CPU,
			Duration: time.Since(alloc.StartTime),
			Tokens:   alloc.APITokens,
		}
		report.Agents = append(report.Agents, usage)
	}

	return report
}

func (rm *ResourceManager) UpdateTokenUsage(agentID string, model string, tokens int) {
	rm.mu.Lock()
	defer rm.mu.Unlock()

	if alloc, ok := rm.allocated[agentID]; ok {
		if alloc.APITokens == nil {
			alloc.APITokens = make(map[string]int)
		}
		alloc.APITokens[model] += tokens
	}
}

func parseMemory(memStr string) int64 {
	memStr = strings.TrimSpace(strings.ToUpper(memStr))
	
	multiplier := int64(1)
	if strings.HasSuffix(memStr, "GI") {
		multiplier = 1024 * 1024 * 1024
		memStr = strings.TrimSuffix(memStr, "GI")
	} else if strings.HasSuffix(memStr, "G") {
		multiplier = 1000 * 1000 * 1000
		memStr = strings.TrimSuffix(memStr, "G")
	} else if strings.HasSuffix(memStr, "MI") {
		multiplier = 1024 * 1024
		memStr = strings.TrimSuffix(memStr, "MI")
	} else if strings.HasSuffix(memStr, "M") {
		multiplier = 1000 * 1000
		memStr = strings.TrimSuffix(memStr, "M")
	}

	value, err := strconv.ParseInt(memStr, 10, 64)
	if err != nil {
		return 512 * 1024 * 1024
	}

	return value * multiplier
}

func formatBytes(bytes int64) string {
	const (
		_  = iota
		KB = 1 << (10 * iota)
		MB
		GB
	)

	switch {
	case bytes >= GB:
		return fmt.Sprintf("%.1fGi", float64(bytes)/GB)
	case bytes >= MB:
		return fmt.Sprintf("%.1fMi", float64(bytes)/MB)
	case bytes >= KB:
		return fmt.Sprintf("%.1fKi", float64(bytes)/KB)
	default:
		return fmt.Sprintf("%d bytes", bytes)
	}
}

func getSystemMemory() int64 {
	return 16 * 1024 * 1024 * 1024
}