package harness

// CacheCreation is the cache_creation usage object of Anthropic-shaped records.
// Older records carry only the total (cache_creation_input_tokens); the split
// into a 1h and a 5m TTL tier was added later and both tiers are a subset of
// the total. A nil *CacheCreation is valid and means "no split recorded".
type CacheCreation struct {
	Ephemeral1h int64 `json:"ephemeral_1h_input_tokens"`
	Ephemeral5m int64 `json:"ephemeral_5m_input_tokens"`
}

// Write1h returns the 1h-tier cache-write subtotal, clamped to total so it can
// never exceed the cache-write class it belongs to.
func (c *CacheCreation) Write1h(total int64) int64 {
	if c == nil || c.Ephemeral1h <= 0 || total <= 0 {
		return 0
	}
	if c.Ephemeral1h > total {
		return total
	}
	return c.Ephemeral1h
}
