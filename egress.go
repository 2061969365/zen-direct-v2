package main

import (
	"context"
	"sync"
	"time"
)

// 出口 IP 缓存：/health 直接读缓存，避免每次探活都去打外部 IP 服务。
// 复用 main.go 里 egressIP() 的多源探测，每 5 分钟刷新一次；
// 刷新失败时保留上一次成功的值，宁可旧也不空。
var egressCache struct {
	mu sync.Mutex
	ip string
	at time.Time
}

func cachedEgressIP() string {
	egressCache.mu.Lock()
	defer egressCache.mu.Unlock()
	return egressCache.ip
}

func setEgressIP(ip string) {
	if ip == "" {
		return
	}
	egressCache.mu.Lock()
	egressCache.ip = ip
	egressCache.at = time.Now()
	egressCache.mu.Unlock()
}

func refreshEgressIP(ctx context.Context) string {
	if ip := egressIP(); ip != "" {
		setEgressIP(ip)
		return ip
	}
	return ""
}

func startEgressResolver(ctx context.Context) {
	go func() {
		for {
			refreshEgressIP(ctx)
			select {
			case <-ctx.Done():
				return
			case <-time.After(5 * time.Minute):
			}
		}
	}()
}
