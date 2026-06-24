package main

import (
	"io"
	"sync"
	"time"
)

// ==================== 简易令牌桶限速 ====================
//
// 不依赖 golang.org/x/time/rate，避免新增外部依赖。
// 实现并发安全，可在多个 reader/writer 之间共享同一个桶以达到全局限速；
// 也可以为单连接创建独立桶实现 per-conn 限速。

// tokenBucket 令牌桶
type tokenBucket struct {
	mu         sync.Mutex
	ratePerSec int64 // 0 表示不限速
	capacity   int64
	tokens     float64
	lastRefill time.Time
}

func newTokenBucket(ratePerSec int64) *tokenBucket {
	if ratePerSec <= 0 {
		return &tokenBucket{}
	}
	// 桶容量 = 1 秒带宽，允许 1s 的突发
	return &tokenBucket{
		ratePerSec: ratePerSec,
		capacity:   ratePerSec,
		tokens:     float64(ratePerSec),
		lastRefill: time.Now(),
	}
}

// wait 阻塞直到能取出 n 个令牌（或上下文取消）
//
// 当 ratePerSec <= 0 时立即返回。
func (b *tokenBucket) wait(n int64) {
	if b == nil || b.ratePerSec <= 0 || n <= 0 {
		return
	}

	for {
		b.mu.Lock()
		now := time.Now()
		elapsed := now.Sub(b.lastRefill).Seconds()
		if elapsed > 0 {
			b.tokens += elapsed * float64(b.ratePerSec)
			if b.tokens > float64(b.capacity) {
				b.tokens = float64(b.capacity)
			}
			b.lastRefill = now
		}

		if b.tokens >= float64(n) {
			b.tokens -= float64(n)
			b.mu.Unlock()
			return
		}

		// 需要等多久才能凑够 n 个令牌
		shortage := float64(n) - b.tokens
		sleep := time.Duration(shortage / float64(b.ratePerSec) * float64(time.Second))
		b.mu.Unlock()

		if sleep < time.Millisecond {
			sleep = time.Millisecond
		}
		time.Sleep(sleep)
	}
}

// throttledReader 在每次 Read 之前等待令牌
type throttledReader struct {
	r io.Reader
	b *tokenBucket
}

func newThrottledReader(r io.Reader, b *tokenBucket) io.Reader {
	if b == nil || b.ratePerSec <= 0 {
		return r
	}
	return &throttledReader{r: r, b: b}
}

func (t *throttledReader) Read(p []byte) (int, error) {
	n, err := t.r.Read(p)
	if n > 0 {
		t.b.wait(int64(n))
	}
	return n, err
}

// throttledWriter 在每次 Write 之前等待令牌
type throttledWriter struct {
	w io.Writer
	b *tokenBucket
}

func newThrottledWriter(w io.Writer, b *tokenBucket) io.Writer {
	if b == nil || b.ratePerSec <= 0 {
		return w
	}
	return &throttledWriter{w: w, b: b}
}

func (t *throttledWriter) Write(p []byte) (int, error) {
	// 大块写按 chunk 分段，避免一次性等待过久导致连接超时
	const chunk = 64 * 1024
	written := 0
	for written < len(p) {
		end := written + chunk
		if end > len(p) {
			end = len(p)
		}
		t.b.wait(int64(end - written))
		n, err := t.w.Write(p[written:end])
		written += n
		if err != nil {
			return written, err
		}
	}
	return written, nil
}
