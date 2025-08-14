package cache

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/go-redis/redis/v8"
)

// CacheLevel 定义缓存级别
type CacheLevel int

const (
	L1Cache CacheLevel = iota // 本地内存缓存
	L2Cache                   // Redis缓存
)

// CacheConfig 缓存配置
type CacheConfig struct {
	EnableL1Cache    bool           // 是否启用本地内存缓存
	EnableL2Cache    bool           // 是否启用Redis缓存
	L1TTL            int64          // 本地缓存默认过期时间(秒)
	L2TTL            int64          // Redis缓存默认过期时间(秒)
	MaxL1Size        int            // 本地缓存最大条目数
	RedisOptions     *redis.Options // Redis配置
	PromotionStrategy PromotionStrategy // 缓存升级策略
	DemotionStrategy  DemotionStrategy  // 缓存降级策略
}

// CacheItem 缓存项
type CacheItem struct {
	Value      interface{} `json:"value"`
	ExpireTime int64       `json:"expire_time"` // 过期时间戳
	CreateTime int64       `json:"create_time"` // 创建时间戳
	AccessTime int64       `json:"access_time"` // 最后访问时间戳
	AccessCount int64      `json:"access_count"` // 访问次数
}

// MultiLevelCache 多级缓存实现
type MultiLevelCache struct {
	config         CacheConfig
	localCache     sync.Map      // 本地内存缓存
	redisClient    *redis.Client // Redis客户端
	mutex          sync.RWMutex  // 读写锁
	ctx            context.Context
	itemCount      int           // 当前本地缓存项数量
	cleanupTicker  *time.Ticker  // 清理过期项的定时器
	stopCleanup    chan struct{} // 停止清理的信号
}

// NewMultiLevelCache 创建新的多级缓存
func NewMultiLevelCache(config CacheConfig) (*MultiLevelCache, error) {
	cache := &MultiLevelCache{
		config:      config,
		ctx:         context.Background(),
		stopCleanup: make(chan struct{}),
	}

	// 初始化Redis客户端(如果启用)
	if config.EnableL2Cache {
		if config.RedisOptions == nil {
			return nil, errors.New("Redis配置不能为空")
		}
		cache.redisClient = redis.NewClient(config.RedisOptions)
		// 测试连接
		_, err := cache.redisClient.Ping(cache.ctx).Result()
		if err != nil {
			return nil, err
		}
	}

	// 如果未设置策略，使用默认策略
	if config.PromotionStrategy == nil {
		cache.config.PromotionStrategy = NewFrequencyBasedStrategy(3, 60, 0)
	}
	
	if config.DemotionStrategy == nil {
		cache.config.DemotionStrategy = NewFrequencyBasedStrategy(0, 0, 300) // 5分钟未访问降级
	}

	// 启动定期清理过期项的协程
	if config.EnableL1Cache {
		cache.cleanupTicker = time.NewTicker(time.Minute) // 每分钟清理一次
		go cache.cleanupRoutine()
	}

	return cache, nil
}

// cleanupRoutine 定期清理过期和需要降级的缓存项
func (c *MultiLevelCache) cleanupRoutine() {
	for {
		select {
		case <-c.cleanupTicker.C:
			c.cleanupExpiredItems()
		case <-c.stopCleanup:
			c.cleanupTicker.Stop()
			return
		}
	}
}

// cleanupExpiredItems 清理过期和需要降级的缓存项
func (c *MultiLevelCache) cleanupExpiredItems() {
	now := time.Now().Unix()
	keysToDelete := make([]string, 0)
	keysToDemote := make([]string, 0)
	
	// 收集需要删除和降级的键
	c.localCache.Range(func(key, value interface{}) bool {
		k := key.(string)
		item := value.(*CacheItem)
		
		// 检查是否过期
		if item.ExpireTime <= now {
			keysToDelete = append(keysToDelete, k)
			return true
		}
		
		// 检查是否需要降级
		if c.config.DemotionStrategy.ShouldDemote(item) {
			keysToDemote = append(keysToDemote, k)
		}
		
		return true
	})
	
	// 删除过期项
	for _, k := range keysToDelete {
		c.localCache.Delete(k)
		c.itemCount--
	}
	
	// 处理需要降级的项
	for _, k := range keysToDemote {
		if v, ok := c.localCache.Load(k); ok {
			item := v.(*CacheItem)
			// 如果启用了L2缓存，将项降级到L2
			if c.config.EnableL2Cache {
				jsonData, err := json.Marshal(item)
				if err == nil {
					ttl := item.ExpireTime - now
					if ttl > 0 {
						c.redisClient.Set(c.ctx, k, jsonData, time.Duration(ttl)*time.Second)
					}
				}
			}
			// 从本地缓存中删除
			c.localCache.Delete(k)
			c.itemCount--
		}
	}
	
	// 如果超过最大大小限制，进行LRU淘汰
	if c.config.MaxL1Size > 0 && c.itemCount > c.config.MaxL1Size {
		c.evictLRU(c.itemCount - c.config.MaxL1Size)
	}
}

// evictLRU 淘汰最近最少使用的缓存项
func (c *MultiLevelCache) evictLRU(count int) {
	type itemWithKey struct {
		key  string
		item *CacheItem
	}
	
	// 收集所有项并按访问时间排序
	items := make([]itemWithKey, 0, c.itemCount)
	c.localCache.Range(func(key, value interface{}) bool {
		k := key.(string)
		item := value.(*CacheItem)
		items = append(items, itemWithKey{key: k, item: item})
		return true
	})
	
	// 按访问时间排序（升序，最早访问的在前面）
	sort.Slice(items, func(i, j int) bool {
		return items[i].item.AccessTime < items[j].item.AccessTime
	})
	
	// 淘汰指定数量的项
	evictCount := count
	if evictCount > len(items) {
		evictCount = len(items)
	}
	
	for i := 0; i < evictCount; i++ {
		k := items[i].key
		item := items[i].item
		
		// 如果启用了L2缓存，将项降级到L2
		if c.config.EnableL2Cache {
			jsonData, err := json.Marshal(item)
			if err == nil {
				ttl := item.ExpireTime - time.Now().Unix()
				if ttl > 0 {
					c.redisClient.Set(c.ctx, k, jsonData, time.Duration(ttl)*time.Second)
				}
			}
		}
		
		// 从本地缓存中删除
		c.localCache.Delete(k)
		c.itemCount--
	}
}

// Set 设置缓存
func (c *MultiLevelCache) Set(key string, value interface{}, ttl int64) error {
	now := time.Now().Unix()
	expireTime := now + ttl
	
	item := &CacheItem{
		Value:      value,
		ExpireTime: expireTime,
		CreateTime: now,
		AccessTime: now,
		AccessCount: 0,
	}

	// 设置本地缓存
	if c.config.EnableL1Cache {
		// 检查是否已存在该键
		if _, exists := c.localCache.Load(key); !exists {
			c.itemCount++
		}
		c.localCache.Store(key, item)
		
		// 如果超过最大大小限制，进行LRU淘汰
		if c.config.MaxL1Size > 0 && c.itemCount > c.config.MaxL1Size {
			c.evictLRU(1) // 淘汰一项
		}
	}

	// 设置Redis缓存
	if c.config.EnableL2Cache {
		jsonData, err := json.Marshal(item)
		if err != nil {
			return err
		}
		
		err = c.redisClient.Set(c.ctx, key, jsonData, time.Duration(ttl)*time.Second).Err()
		if err != nil {
			return err
		}
	}

	return nil
}

// Get 获取缓存
func (c *MultiLevelCache) Get(key string) (interface{}, bool) {
	now := time.Now().Unix()
	
	// 优先从本地缓存获取
	if c.config.EnableL1Cache {
		if val, ok := c.localCache.Load(key); ok {
			item := val.(*CacheItem)
			
			// 检查是否过期
			if item.ExpireTime > now {
				// 更新访问信息
				item.AccessTime = now
				item.AccessCount++
				c.localCache.Store(key, item)
				return item.Value, true
			} else {
				// 过期了，删除
				c.localCache.Delete(key)
				c.itemCount--
			}
		}
	}

	// 如果本地缓存未命中或已过期，尝试从Redis获取
	if c.config.EnableL2Cache {
		jsonData, err := c.redisClient.Get(c.ctx, key).Bytes()
		if err != nil {
			if err == redis.Nil {
				return nil, false
			}
			// Redis错误，返回未命中
			return nil, false
		}

		var item CacheItem
		if err := json.Unmarshal(jsonData, &item); err != nil {
			return nil, false
		}

		// 检查是否过期(理论上Redis会自动过期，这里是双重检查)
		if item.ExpireTime > now {
			// 更新访问信息
			item.AccessTime = now
			item.AccessCount++
			
			// 考虑是否需要升级到本地缓存
			if c.config.EnableL1Cache && c.config.PromotionStrategy.ShouldPromote(&item) {
				// 将项从L2升级到L1
				c.localCache.Store(key, &item)
				c.itemCount++
				
				// 如果超过最大大小限制，进行LRU淘汰
				if c.config.MaxL1Size > 0 && c.itemCount > c.config.MaxL1Size {
					c.evictLRU(1) // 淘汰一项
				}
			}
			
			// 更新Redis中的访问信息
			jsonData, _ := json.Marshal(item)
			c.redisClient.Set(c.ctx, key, jsonData, time.Duration(item.ExpireTime-now)*time.Second)
			
			return item.Value, true
		}
	}

	return nil, false
}

// Delete 删除缓存
func (c *MultiLevelCache) Delete(key string) error {
	// 删除本地缓存
	if c.config.EnableL1Cache {
		if _, exists := c.localCache.Load(key); exists {
			c.localCache.Delete(key)
			c.itemCount--
		}
	}

	// 删除Redis缓存
	if c.config.EnableL2Cache {
		err := c.redisClient.Del(c.ctx, key).Err()
		if err != nil {
			return err
		}
	}

	return nil
}

// Clear 清空所有缓存
func (c *MultiLevelCache) Clear() error {
	// 清空本地缓存
	if c.config.EnableL1Cache {
		c.localCache = sync.Map{}
		c.itemCount = 0
	}

	// 清空Redis缓存(谨慎使用，这会清空整个Redis)
	if c.config.EnableL2Cache {
		err := c.redisClient.FlushDB(c.ctx).Err()
		if err != nil {
			return err
		}
	}

	return nil
}

// GetWithTTL 获取缓存并返回剩余TTL
func (c *MultiLevelCache) GetWithTTL(key string) (interface{}, int64, bool) {
	now := time.Now().Unix()
	
	// 优先从本地缓存获取
	if c.config.EnableL1Cache {
		if val, ok := c.localCache.Load(key); ok {
			item := val.(*CacheItem)
			
			// 检查是否过期
			if item.ExpireTime > now {
				// 计算剩余TTL
				ttl := item.ExpireTime - now
				
				// 更新访问信息
				item.AccessTime = now
				item.AccessCount++
				c.localCache.Store(key, item)
				
				return item.Value, ttl, true
			} else {
				// 过期了，删除
				c.localCache.Delete(key)
				c.itemCount--
			}
		}
	}

	// 如果本地缓存未命中或已过期，尝试从Redis获取
	if c.config.EnableL2Cache {
		// 获取TTL
		ttl, err := c.redisClient.TTL(c.ctx, key).Result()
		if err != nil || ttl <= 0 {
			return nil, 0, false
		}
		
		// 获取值
		jsonData, err := c.redisClient.Get(c.ctx, key).Bytes()
		if err != nil {
			return nil, 0, false
		}

		var item CacheItem
		if err := json.Unmarshal(jsonData, &item); err != nil {
			return nil, 0, false
		}

		// 更新访问信息
		item.AccessTime = now
		item.AccessCount++
		
		// 考虑是否需要升级到本地缓存
		if c.config.EnableL1Cache && c.config.PromotionStrategy.ShouldPromote(&item) {
			// 将项从L2升级到L1
			c.localCache.Store(key, &item)
			c.itemCount++
			
			// 如果超过最大大小限制，进行LRU淘汰
			if c.config.MaxL1Size > 0 && c.itemCount > c.config.MaxL1Size {
				c.evictLRU(1) // 淘汰一项
			}
		}
		
		// 更新Redis中的访问信息
		jsonData, _ = json.Marshal(item)
		c.redisClient.Set(c.ctx, key, jsonData, ttl)
		
		return item.Value, int64(ttl.Seconds()), true
	}

	return nil, 0, false
}

// SetWithExpiration 设置缓存并指定过期时间
func (c *MultiLevelCache) SetWithExpiration(key string, value interface{}, expiration time.Time) error {
	now := time.Now().Unix()
	expireTime := expiration.Unix()
	
	// 如果过期时间已过，不设置缓存
	if expireTime <= now {
		return nil
	}
	
	ttl := expireTime - now
	return c.Set(key, value, ttl)
}

// GetStats 获取缓存统计信息
func (c *MultiLevelCache) GetStats() map[string]interface{} {
	stats := make(map[string]interface{})
	
	// 本地缓存统计
	if c.config.EnableL1Cache {
		stats["l1_item_count"] = c.itemCount
		stats["l1_max_size"] = c.config.MaxL1Size
	}
	
	// Redis统计(如果启用)
	if c.config.EnableL2Cache {
		// 获取Redis信息
		info, err := c.redisClient.Info(c.ctx).Result()
		if err == nil {
			stats["redis_info"] = info
		}
		
		// 获取Redis键数量
		dbSize, err := c.redisClient.DBSize(c.ctx).Result()
		if err == nil {
			stats["redis_key_count"] = dbSize
		}
	}
	
	return stats
}

// Close 关闭缓存连接
func (c *MultiLevelCache) Close() error {
	// 停止清理协程
	if c.cleanupTicker != nil {
		close(c.stopCleanup)
	}
	
	// 关闭Redis连接
	if c.config.EnableL2Cache && c.redisClient != nil {
		return c.redisClient.Close()
	}
	
	return nil
}
