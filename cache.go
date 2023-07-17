package cache

import (
	"context"
	"fmt"
	"reflect"
	"sync"
	"time"

	"github.com/gomodule/redigo/redis"
	"github.com/pkg/errors"
	"github.com/seaguest/deepcopy"
	"golang.org/x/sync/singleflight"
)

var (
	ErrIllegalTTL = errors.New("illegal ttl, must be in whole numbers of seconds, no fractions")
)

const (
	defaultNamespace = "default"
)

type Cache interface {
	SetObject(ctx context.Context, key string, obj interface{}, ttl time.Duration) error

	// GetObject loader function f() will be called in case cache all miss
	// suggest to use object_type#id as key or any other pattern which can easily extract object, aggregate metric for same object in onMetric
	GetObject(ctx context.Context, key string, obj interface{}, ttl time.Duration, f func() (interface{}, error)) error

	Delete(key string) error

	// Disable GetObject will call loader function in case cache is disabled.
	Disable()

	// DeleteFromMem allows to delete key from mem, for test purpose
	DeleteFromMem(key string)

	// DeleteFromRedis allows to delete key from redis, for test purpose
	DeleteFromRedis(key string) error
}

type cache struct {
	options Options

	// store the pkg_path+type<->object mapping
	types sync.Map

	// rds cache, handles redis level cache
	rds *redisCache

	// mem cache, handles in-memory cache
	mem *memCache

	sfg singleflight.Group

	metric Metrics
}

func New(options ...Option) Cache {
	c := &cache{}
	opts := newOptions(options...)

	// set default namespace if missing
	if opts.Namespace == "" {
		opts.Namespace = defaultNamespace
	}

	// set separator
	if opts.Separator == "" {
		panic("Separator unspecified")
	}

	// set default RedisTTLFactor to 4 if missing
	if opts.RedisTTLFactor == 0 {
		opts.RedisTTLFactor = 4
	}

	// set default CleanInterval to 10s if missing
	if opts.CleanInterval == 0 {
		opts.CleanInterval = time.Second * 10
	} else if opts.CleanInterval < time.Second {
		panic("CleanInterval must be second at least")
	}

	if opts.OnError == nil {
		panic("OnError is nil")
	}

	c.options = opts
	c.metric = opts.Metric
	c.metric.namespace = opts.Namespace
	c.mem = newMemCache(opts.CleanInterval, c.metric)
	c.rds = newRedisCache(opts.GetConn, opts.RedisTTLFactor, c.metric)
	go c.watch()
	return c
}

// Disable , disable cache, call loader function for each call
func (c *cache) Disable() {
	c.options.Disabled = true
}

func (c *cache) SetObject(ctx context.Context, key string, obj interface{}, ttl time.Duration) error {
	done := make(chan error)
	var err error
	go func() {
		done <- c.setObject(key, obj, ttl)
	}()

	select {
	case err = <-done:
	case <-ctx.Done():
		err = errors.WithStack(ctx.Err())
	}
	return err
}

func (c *cache) setObject(key string, obj interface{}, ttl time.Duration) (err error) {
	if ttl > ttl.Truncate(time.Second) {
		return errors.WithStack(ErrIllegalTTL)
	}

	typeName := getTypeName(obj)
	c.checkType(typeName, obj, ttl)

	namespacedKey := c.namespacedKey(key)
	defer c.metric.Observe()(namespacedKey, MetricTypeSetCache, &err)

	_, err, _ = c.sfg.Do(namespacedKey+"_set", func() (interface{}, error) {
		_, err := c.rds.set(namespacedKey, obj, ttl)
		if err != nil {
			return nil, errors.WithStack(err)
		}

		c.notifyAll(&actionRequest{
			Action:   cacheSet,
			TypeName: typeName,
			Key:      namespacedKey,
			Object:   obj,
		})
		return nil, nil
	})
	return err
}

func (c *cache) GetObject(ctx context.Context, key string, obj interface{}, ttl time.Duration, f func() (interface{}, error)) error {
	// is disabled, call loader function
	if c.options.Disabled {
		o, err := f()
		if err != nil {
			return err
		}
		return c.copy(o, obj)
	}

	done := make(chan error)
	var err error
	go func() {
		done <- c.getObject(key, obj, ttl, f)
	}()

	select {
	case err = <-done:
	case <-ctx.Done():
		err = errors.WithStack(ctx.Err())
	}
	return err
}

func (c *cache) getObject(key string, obj interface{}, ttl time.Duration, f func() (interface{}, error)) (err error) {
	if ttl > ttl.Truncate(time.Second) {
		return errors.WithStack(ErrIllegalTTL)
	}

	typeName := getTypeName(obj)
	c.checkType(typeName, obj, ttl)

	var expired bool
	namespacedKey := c.namespacedKey(key)
	defer c.metric.Observe()(namespacedKey, MetricTypeGetCache, &err)

	var it *Item
	defer func() {
		// deepcopy before return
		if err == nil {
			err = c.copy(it.Object, obj)
		}

		// if hit but expired, then do a fresh load
		if expired {
			go func() {
				// async load metric
				defer c.metric.Observe()(namespacedKey, MetricTypeAsyncLoad, nil)

				_, resetErr := c.resetObject(namespacedKey, ttl, f)
				if resetErr != nil {
					c.options.OnError(errors.WithStack(resetErr))
					return
				}
			}()
		}
	}()

	// try to retrieve from local cache, return if found
	it = c.mem.get(namespacedKey)
	if it != nil {
		if it.Expired() {
			expired = true
		}
		return
	}

	var itf interface{}
	itf, err, _ = c.sfg.Do(namespacedKey+"_get", func() (interface{}, error) {
		// try to retrieve from redis, return if found
		v, redisErr := c.rds.get(namespacedKey, obj)
		if redisErr != nil {
			return nil, errors.WithStack(redisErr)
		}
		if v != nil {
			if v.Expired() {
				expired = true
			} else {
				// update memory cache since it is not previously found in mem
				c.mem.set(namespacedKey, v)
			}
			return v, nil
		}
		return c.resetObject(namespacedKey, ttl, f)
	})
	if err != nil {
		return
	}
	it = itf.(*Item)
	return
}
