// © 2022 Nokia.
//
// This code is a Contribution to the gNMIc project (“Work”) made under the Google Software Grant and Corporate Contributor License Agreement (“CLA”) and governed by the Apache License 2.0.
// No other rights or licenses in or to any of Nokia’s intellectual property are granted for any other purpose.
// This code is provided on an “as is” basis without any warranties of any kind.
//
// SPDX-License-Identifier: Apache-2.0

package cache

import (
	"context"
	"fmt"
	"log"
	"os"
	"sync"
	"time"

	redis "github.com/go-redis/redis/v8"
	"github.com/openconfig/gnmi/proto/gnmi"
	"github.com/openconfig/gnmic/utils"
	"google.golang.org/protobuf/proto"
)

const (
	loggingPrefixRedis   = "[cache:redis] "
	cacheChannelsChannel = "gnmic_cache_channels"
	defaultRedisAddress  = "127.0.0.1:6379"
)

type redisCache struct {
	cfg *Config
	oc  *gnmiCache
	cfn context.CancelFunc

	c           *redis.Client
	channelChan chan string
	m           *sync.RWMutex
	channels    map[string]struct{}
	logger      *log.Logger
}

func newRedisCache(cfg *Config, opts ...Option) (*redisCache, error) {
	if cfg == nil {
		cfg = &Config{
			Type:    cacheType_Redis,
			Address: defaultRedisAddress,
		}
	}
	cfg.setDefaults()

	c := &redisCache{
		cfg:         cfg,
		oc:          newGNMICache(cfg, "redis", opts...),
		channelChan: make(chan string),
		m:           new(sync.RWMutex),
		channels:    make(map[string]struct{}),
	}

	for _, opt := range opts {
		opt(c)
	}
	if c.logger == nil {
		c.logger = log.New(os.Stderr, loggingPrefixRedis, utils.DefaultLoggingFlags)
	}
CLIENT:
	c.c = redis.NewClient(&redis.Options{
		Addr:     cfg.Address,
		Username: cfg.Username,
		Password: cfg.Password,
		DB:       0,
	})

	ctx, cancel := context.WithCancel(context.Background())
	c.cfn = cancel

	pong, err := c.c.Ping(ctx).Result()
	if err != nil {
		c.logger.Printf("failed to connect to redis: %v", err)
		time.Sleep(time.Second)
		goto CLIENT
	}

	c.logger.Printf("ping result: %s", pong)
	go c.sync(ctx)
	return c, nil
}

func (c *redisCache) SetLogger(logger *log.Logger) {
	if logger != nil && c.logger != nil {
		c.logger.SetOutput(logger.Writer())
		c.logger.SetFlags(logger.Flags())
		c.logger.SetPrefix(loggingPrefixRedis)
	}
}

func (c *redisCache) Write(ctx context.Context, subscriptionName string, m proto.Message) {
	// write the msg to redis
	c.writeRemoteREDIS(ctx, subscriptionName, m)
	// publish the subscription name to redis for other gnmic instances
	var ok bool
	c.m.RLock()
	defer func() {
		c.m.RUnlock()
		if !ok {
			c.m.Lock()
			c.channels[subscriptionName] = struct{}{}
			c.m.Unlock()
			c.c.Publish(ctx, cacheChannelsChannel, []byte(subscriptionName))
		}
	}()
	_, ok = c.channels[subscriptionName]
}

func (c *redisCache) writeRemoteREDIS(ctx context.Context, subscriptionName string, m proto.Message) {
	switch m := m.ProtoReflect().Interface().(type) {
	case *gnmi.SubscribeResponse:
		switch rsp := m.GetResponse().(type) {
		case *gnmi.SubscribeResponse_Update:
			targetName := rsp.Update.GetPrefix().GetTarget()
			if targetName == "" {
				c.logger.Printf("subscription=%q: response missing target: %v", subscriptionName, rsp)
				return
			}
			c.channelChan <- subscriptionName
			var err error
			err = c.publishNotificationREDIS(ctx, subscriptionName, targetName, m)
			if err != nil {
				c.logger.Print(err)
			}
		}
	}
}

func (c *redisCache) publishNotificationREDIS(ctx context.Context, subscriptionName, targetName string, r *gnmi.SubscribeResponse) error {
	ctx, cancel := context.WithTimeout(ctx, c.cfg.Timeout)
	defer cancel()

	b, err := proto.Marshal(r)
	if err != nil {
		return fmt.Errorf("failed to marshal proto message: %w", err)
	}
	status := c.c.Publish(ctx, fmt.Sprintf("%s.%s", subscriptionName, targetName), b)
	if status.Err() != nil {
		err = fmt.Errorf("failed to publish statusErr: %v", status.Err())
		c.logger.Print(err)
		return err
	}
	_, err = status.Result()
	if err != nil {
		err = fmt.Errorf("failed to publish resultErr: %v", err)
		c.logger.Print(err)
	}
	return nil
}

func (c *redisCache) Read() (map[string][]*gnmi.Notification, error) {
	return c.oc.Read()
}

func (c *redisCache) sync(ctx context.Context) {
	c.logger.Printf("start redis sync")
	// subscribe to cache channel updates
	// and periodically reset the local channels map.
	go func() {
		ticker := time.NewTicker(subjectCacheResetPeriod)
		channelSub := c.c.Subscribe(ctx, cacheChannelsChannel)
		defer channelSub.Close()

		for {
			select {
			case <-ctx.Done():
				return
			case msg := <-channelSub.Channel():
				// pass the channel name to start syncChannel func
				c.channelChan <- msg.Payload
			case <-ticker.C:
				// reset local channels map to re trigger broadcast
				c.m.Lock()
				c.channels = make(map[string]struct{})
				c.m.Unlock()
			}
		}
	}()

	// keeps track of channels for which a syncChannel has been started
	channels := make(map[string]struct{})
	for {
		select {
		case <-ctx.Done():
			return
		case cc := <-c.channelChan:
			c.m.Lock()
			if _, ok := channels[cc]; !ok {
				channels[cc] = struct{}{}
				c.logger.Printf("starting redis channel %q sync", cc)
				go c.syncChannel(ctx, cc)
			}
			c.m.Unlock()
		}
	}
}

// syncChannel subscribes to redis channel updates and syncs the local cache
func (c *redisCache) syncChannel(ctx context.Context, channel string) {
	sub := c.c.PSubscribe(ctx, fmt.Sprintf("%s*", channel))
	defer sub.Close()
	i := 0
	for {
		select {
		case msg := <-sub.Channel():
			if len(msg.Payload) == 0 {
				continue
			}
			m := new(gnmi.SubscribeResponse)
			err := proto.Unmarshal([]byte(msg.Payload), m)
			if err != nil {
				c.logger.Printf("failed to unmarshal proto msg: %v", err)
				continue
			}
			c.oc.Write(ctx, channel, m)
			i++
		case <-ctx.Done():
			return
		}
	}
}

func (c *redisCache) Subscribe(ctx context.Context, ro *ReadOpts) chan *Notification {
	return c.oc.Subscribe(ctx, ro)
}

func (c *redisCache) Stop() {
	c.cfn()
	if c.c != nil {
		c.c.Close()
	}
}

func (c *redisCache) DeleteTarget(name string) {
	c.oc.DeleteTarget(name)
}
