package cache

import (
	"time"
)

// PromotionStrategy 缓存升级策略接口
type PromotionStrategy interface {
	// ShouldPromote 决定是否应该将缓存项从L2升级到L1
	ShouldPromote(item *CacheItem) bool
}

// DemotionStrategy 缓存降级策略接口
type DemotionStrategy interface {
	// ShouldDemote 决定是否应该将缓存项从L1降级到L2
	ShouldDemote(item *CacheItem) bool
}

// FrequencyBasedStrategy 基于访问频率的策略
type FrequencyBasedStrategy struct {
	accessThreshold int64 // 访问次数阈值
	timeWindow      int64 // 时间窗口(秒)
	idleTime        int64 // 空闲时间阈值(秒)
}

// NewFrequencyBasedStrategy 创建新的基于频率的策略
func NewFrequencyBasedStrategy(accessThreshold, timeWindow, idleTime int64) *FrequencyBasedStrategy {
	return &FrequencyBasedStrategy{
		accessThreshold: accessThreshold,
		timeWindow:      timeWindow,
		idleTime:        idleTime,
	}
}

// ShouldPromote 判断是否应该升级缓存
func (s *FrequencyBasedStrategy) ShouldPromote(item *CacheItem) bool {
	now := time.Now().Unix()
	
	// 如果设置了访问次数阈值和时间窗口
	if s.accessThreshold > 0 && s.timeWindow > 0 {
		// 在指定时间窗口内，访问次数超过阈值则升级
		timeInWindow := now - item.CreateTime
		if timeInWindow <= s.timeWindow && item.AccessCount >= s.accessThreshold {
			return true
		}
	}
	
	return false
}

// ShouldDemote 判断是否应该降级缓存
func (s *FrequencyBasedStrategy) ShouldDemote(item *CacheItem) bool {
	now := time.Now().Unix()
	
	// 如果设置了空闲时间阈值
	if s.idleTime > 0 {
		// 超过空闲时间未访问则降级
		idleTime := now - item.AccessTime
		if idleTime >= s.idleTime {
			return true
		}
	}
	
	return false
}

// TimeWindowStrategy 基于时间窗口的策略
type TimeWindowStrategy struct {
	accessThreshold int64 // 时间窗口内的访问次数阈值
	timeWindow      int64 // 时间窗口(秒)
	idleThreshold   int64 // 空闲时间阈值(秒)
}

// NewTimeWindowStrategy 创建新的基于时间窗口的策略
func NewTimeWindowStrategy(accessThreshold, timeWindow, idleThreshold int64) *TimeWindowStrategy {
	return &TimeWindowStrategy{
		accessThreshold: accessThreshold,
		timeWindow:      timeWindow,
		idleThreshold:   idleThreshold,
	}
}

// ShouldPromote 判断是否应该升级缓存
func (s *TimeWindowStrategy) ShouldPromote(item *CacheItem) bool {
	now := time.Now().Unix()
	
	// 在最近的时间窗口内，访问次数超过阈值则升级
	if s.timeWindow > 0 && s.accessThreshold > 0 {
		windowStart := now - s.timeWindow
		if item.AccessTime >= windowStart && item.AccessCount >= s.accessThreshold {
			return true
		}
	}
	
	return false
}

// ShouldDemote 判断是否应该降级缓存
func (s *TimeWindowStrategy) ShouldDemote(item *CacheItem) bool {
	now := time.Now().Unix()
	
	// 超过空闲时间阈值未访问则降级
	if s.idleThreshold > 0 {
		idleTime := now - item.AccessTime
		if idleTime >= s.idleThreshold {
			return true
		}
	}
	
	return false
}

// HybridStrategy 混合策略，支持多种策略组合
type HybridStrategy struct {
	strategies []interface{} // 可以是PromotionStrategy或DemotionStrategy
	requireAll bool          // 是否要求所有策略都满足
}

// NewHybridPromotionStrategy 创建新的混合升级策略
func NewHybridPromotionStrategy(requireAll bool, strategies ...PromotionStrategy) *HybridStrategy {
	interfaceStrategies := make([]interface{}, len(strategies))
	for i, s := range strategies {
		interfaceStrategies[i] = s
	}
	return &HybridStrategy{
		strategies: interfaceStrategies,
		requireAll: requireAll,
	}
}

// NewHybridDemotionStrategy 创建新的混合降级策略
func NewHybridDemotionStrategy(requireAll bool, strategies ...DemotionStrategy) *HybridStrategy {
	interfaceStrategies := make([]interface{}, len(strategies))
	for i, s := range strategies {
		interfaceStrategies[i] = s
	}
	return &HybridStrategy{
		strategies: interfaceStrategies,
		requireAll: requireAll,
	}
}

// ShouldPromote 判断是否应该升级缓存
func (s *HybridStrategy) ShouldPromote(item *CacheItem) bool {
	if len(s.strategies) == 0 {
		return false
	}
	
	if s.requireAll {
		// 所有策略都必须满足
		for _, strategy := range s.strategies {
			if ps, ok := strategy.(PromotionStrategy); ok {
				if !ps.ShouldPromote(item) {
					return false
				}
			}
		}
		return true
	} else {
		// 任一策略满足即可
		for _, strategy := range s.strategies {
			if ps, ok := strategy.(PromotionStrategy); ok {
				if ps.ShouldPromote(item) {
					return true
				}
			}
		}
		return false
	}
}

// ShouldDemote 判断是否应该降级缓存
func (s *HybridStrategy) ShouldDemote(item *CacheItem) bool {
	if len(s.strategies) == 0 {
		return false
	}
	
	if s.requireAll {
		// 所有策略都必须满足
		for _, strategy := range s.strategies {
			if ds, ok := strategy.(DemotionStrategy); ok {
				if !ds.ShouldDemote(item) {
					return false
				}
			}
		}
		return true
	} else {
		// 任一策略满足即可
		for _, strategy := range s.strategies {
			if ds, ok := strategy.(DemotionStrategy); ok {
				if ds.ShouldDemote(item) {
					return true
				}
			}
		}
		return false
	}
}
