package gateway

import (
	"container/list"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sync"
	"time"
)

type cacheEntry struct {
	key       string
	expiresAt time.Time
	record    CacheRecord
}

type MemoryCache struct {
	ttl        time.Duration
	maxEntries int
	items      map[string]*list.Element
	order      *list.List
}

func NewMemoryCache(ttlSeconds, maxEntries int) *MemoryCache {
	return &MemoryCache{
		ttl:        time.Duration(ttlSeconds) * time.Second,
		maxEntries: max(1, maxEntries),
		items:      map[string]*list.Element{},
		order:      list.New(),
	}
}

func (c *MemoryCache) Get(key string) (CacheRecord, bool) {
	el := c.items[key]
	if el == nil {
		return CacheRecord{}, false
	}
	entry := el.Value.(*cacheEntry)
	if time.Now().After(entry.expiresAt) {
		c.order.Remove(el)
		delete(c.items, key)
		return CacheRecord{}, false
	}
	c.order.MoveToBack(el)
	return entry.record, true
}

func (c *MemoryCache) Set(key string, record CacheRecord) {
	if el := c.items[key]; el != nil {
		entry := el.Value.(*cacheEntry)
		entry.record = record
		entry.expiresAt = time.Now().Add(c.ttl)
		c.order.MoveToBack(el)
		return
	}
	el := c.order.PushBack(&cacheEntry{key: key, expiresAt: time.Now().Add(c.ttl), record: record})
	c.items[key] = el
	for len(c.items) > c.maxEntries {
		front := c.order.Front()
		if front == nil {
			break
		}
		entry := front.Value.(*cacheEntry)
		delete(c.items, entry.key)
		c.order.Remove(front)
	}
}

func (c *MemoryCache) Clear() int {
	n := len(c.items)
	c.items = map[string]*list.Element{}
	c.order.Init()
	return n
}

func (c *MemoryCache) Size() int { return len(c.items) }

type CacheLayer struct {
	Config       CacheConfig
	Enabled      bool
	Salt         string
	backend      *MemoryCache
	Hits         int
	Misses       int
	TokensSaved  int
	FamilyHits   map[string]int
	FamilyMisses map[string]int
	mu           sync.Mutex
}

func NewCacheLayer(config CacheConfig) *CacheLayer {
	return &CacheLayer{
		Config:       config,
		Enabled:      config.enabled(),
		Salt:         firstNonEmpty(config.Salt, "change-me"),
		backend:      NewMemoryCache(config.TTLSeconds, config.MaxEntries),
		FamilyHits:   map[string]int{},
		FamilyMisses: map[string]int{},
	}
}

func (c *CacheLayer) MakeKey(namespace, model string, messages []NeutralMessage, params map[string]any) string {
	msgs := make([]map[string]any, 0, len(messages))
	for _, message := range messages {
		msgs = append(msgs, map[string]any{"role": message.Role, "content": message.Content})
	}
	payload := map[string]any{
		"model":    model,
		"messages": msgs,
		"params":   compactMap(params),
	}
	blob, _ := json.Marshal(payload)
	sum := sha256.Sum256([]byte(c.Salt + "\x00" + string(blob)))
	return namespace + ":" + hex.EncodeToString(sum[:])
}

func (c *CacheLayer) Lookup(key, control, model string) *CacheRecord {
	if !c.Enabled || control == "no-store" || control == "no-cache" {
		return nil
	}
	family := modelFamily(model)
	c.mu.Lock()
	defer c.mu.Unlock()
	record, ok := c.backend.Get(key)
	if !ok {
		c.Misses++
		c.FamilyMisses[family]++
		return nil
	}
	c.Hits++
	c.FamilyHits[family]++
	c.TokensSaved += record.Usage.Normalized().TotalTokens
	return &record
}

func (c *CacheLayer) Store(key string, record CacheRecord, control string) {
	if !c.Enabled || control == "no-store" {
		return
	}
	record.Usage = record.Usage.Normalized()
	for i := range record.ToolCalls {
		record.ToolCalls[i] = record.ToolCalls[i].normalized()
	}
	c.mu.Lock()
	c.backend.Set(key, record)
	c.mu.Unlock()
}

func (c *CacheLayer) Stats() map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	total := c.Hits + c.Misses
	byFamily := map[string]any{}
	families := map[string]bool{}
	for fam := range c.FamilyHits {
		families[fam] = true
	}
	for fam := range c.FamilyMisses {
		families[fam] = true
	}
	for fam := range families {
		hits, misses := c.FamilyHits[fam], c.FamilyMisses[fam]
		famTotal := hits + misses
		rate := 0.0
		if famTotal > 0 {
			rate = float64(hits) / float64(famTotal)
		}
		byFamily[fam] = map[string]any{"hits": hits, "misses": misses, "hit_rate": rate}
	}
	rate := 0.0
	if total > 0 {
		rate = float64(c.Hits) / float64(total)
	}
	return map[string]any{
		"enabled":      c.Enabled,
		"backend":      "MemoryCache",
		"hits":         c.Hits,
		"misses":       c.Misses,
		"hit_rate":     rate,
		"tokens_saved": c.TokensSaved,
		"size":         c.backend.Size(),
		"by_family":    byFamily,
	}
}

func (c *CacheLayer) Clear() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.backend.Clear()
}
