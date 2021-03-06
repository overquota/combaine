package juggler

import (
	"context"
	"sync"
	"time"

	"github.com/combaine/combaine/common/logger"
)

// item holds cached item
type itemType struct {
	expires time.Time
	value   []byte
	err     error
	ready   chan struct{}
}

// cache is ttl cache for juggler api responses about checks
type cache struct {
	sync.RWMutex
	ttl        time.Duration
	interval   time.Duration
	store      map[string]*itemType
	runCleaner sync.Once
}

// GlobalCache is singleton for juggler sender
var GlobalCache = &cache{
	ttl:      time.Minute,
	interval: time.Minute * 5,
	store:    make(map[string]*itemType),
}

// TuneCache tune cache ttl and interval
func (c *cache) TuneCache(ttl time.Duration, interval time.Duration) {
	c.Lock()
	c.ttl = ttl
	c.interval = interval
	c.Unlock()
}

type fetcher func(ctx context.Context, id, q string, hosts []string) ([]byte, error)

// Get return not expired element from cacahe or nil
func (c *cache) Get(ctx context.Context, key string, f fetcher, id, q string, hosts []string) ([]byte, error) {
	c.Lock()
	item := c.store[key]
	if item == nil {
		item = &itemType{
			ready:   make(chan struct{}),
			expires: time.Now().Add(c.ttl),
		}
		c.store[key] = item
		c.Unlock()
		item.value, item.err = f(ctx, id, q, hosts)
		if item.err != nil {
			c.Lock()
			delete(c.store, key)
			c.Unlock()
		}
		close(item.ready)
	} else {
		c.Unlock()
		<-item.ready
		logger.Debugf("%s Use cached check for %s", id, key)
	}
	c.runCleaner.Do(func() {
		logger.Debugf("%s run cache cleaner", id)
		go c.cleaner()
	})
	if time.Now().Sub(item.expires) >= 0 {
		logger.Debugf("%s remove stale cached check for %s", id, key)
		c.Lock()
		delete(c.store, key)
		c.Unlock()
	}
	return item.value, item.err
}

// Delete add new element in cache
func (c *cache) Delete(key string) {
	c.Lock()
	delete(c.store, key)
	c.Unlock()
}

func (c *cache) cleaner() {
	var interval time.Duration
	for {
		c.RLock()
		interval = c.interval
		c.RUnlock()
		time.Sleep(interval)
		var staleItems []string
		c.RLock()
		for key, item := range c.store {
			if time.Now().Sub(item.expires) > 0 {
				staleItems = append(staleItems, key)
			}
		}
		c.RUnlock()
		if len(staleItems) > 0 {
			c.Lock()
			for _, k := range staleItems {
				delete(c.store, k)
			}
			c.Unlock()
		}
	}
}
