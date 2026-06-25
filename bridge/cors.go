package main

import (
	"crypto/subtle"
	"net/http"
	"strings"
)

// ==================== Done channel 管理 ====================

// getOrCreateDoneChLocked 返回 authToken 对应的 done channel（不存在则创建）。
// 调用方必须持有 ffb.mu（写锁）。
func (ffb *FileFlowBridge) getOrCreateDoneChLocked(authToken string) chan struct{} {
	if ch, ok := ffb.downloadDone[authToken]; ok {
		return ch
	}
	ch := make(chan struct{})
	ffb.downloadDone[authToken] = ch
	return ch
}

// closeDoneChLocked 关闭并删除 authToken 对应的 done channel（幂等）。
// 调用方必须持有 ffb.mu（写锁）。
func (ffb *FileFlowBridge) closeDoneChLocked(authToken string) {
	if ch, ok := ffb.downloadDone[authToken]; ok {
		select {
		case <-ch:
			// 已 close
		default:
			close(ch)
		}
		delete(ffb.downloadDone, authToken)
	}
}

// ==================== CORS / API Key ====================

// isWildcardOrigin 判断 AllowedOrigins 是否表示"全部放行"
func (ffb *FileFlowBridge) isWildcardOrigin() bool {
	if len(ffb.AllowedOrigins) == 0 {
		return true
	}
	for _, o := range ffb.AllowedOrigins {
		if o == "*" {
			return true
		}
	}
	return false
}

// isOriginAllowed 判断给定 origin 是否被允许
//
// 规则：
//   - AllowedOrigins 为空 或 包含 "*" → 允许所有 origin
//   - 否则精确匹配；空 origin 视为同源请求一律放行
func (ffb *FileFlowBridge) isOriginAllowed(origin string) bool {
	if ffb.isWildcardOrigin() {
		return true
	}
	if origin == "" {
		return true
	}
	for _, o := range ffb.AllowedOrigins {
		if o == origin {
			return true
		}
	}
	return false
}

// requireAPIKey 在 APIKey 配置非空时校验 X-API-Key 或 Authorization: Bearer
func (ffb *FileFlowBridge) requireAPIKey(w http.ResponseWriter, r *http.Request) bool {
	return ffb.requireKey(w, r, ffb.APIKey, "X-API-Key")
}

// requireMetricsKey 校验 /metrics 端点访问凭证
//
// 优先用 MetricsKey；若未配置则放行。**故意与 APIKey 隔离**，避免运维抓取
// 指标时拿到的凭证可以滥用业务接口。
func (ffb *FileFlowBridge) requireMetricsKey(w http.ResponseWriter, r *http.Request) bool {
	if ffb.MetricsKey == "" {
		return true
	}
	return ffb.requireKey(w, r, ffb.MetricsKey, "X-Metrics-Key")
}

// requireKey 通用 key 校验：先看 expectedHeader，再退回 Authorization: Bearer，
// 用常量时间比较防计时侧信道
func (ffb *FileFlowBridge) requireKey(w http.ResponseWriter, r *http.Request, expected, headerName string) bool {
	if expected == "" {
		return true
	}
	got := r.Header.Get(headerName)
	if got == "" {
		auth := r.Header.Get("Authorization")
		if strings.HasPrefix(auth, "Bearer ") {
			got = strings.TrimPrefix(auth, "Bearer ")
		}
	}
	if len(got) != len(expected) || subtle.ConstantTimeCompare([]byte(got), []byte(expected)) != 1 {
		http.Error(w, "无效或缺失的 "+headerName, http.StatusUnauthorized)
		return false
	}
	return true
}

// corsMiddleware 根据 AllowedOrigins 设置响应头并处理预检
func (ffb *FileFlowBridge) corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if ffb.isOriginAllowed(origin) {
			if ffb.isWildcardOrigin() {
				w.Header().Set("Access-Control-Allow-Origin", "*")
			} else if origin != "" {
				w.Header().Set("Access-Control-Allow-Origin", origin)
				w.Header().Set("Vary", "Origin")
			}
			w.Header().Set("Access-Control-Allow-Methods", "GET, HEAD, POST, PUT, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Content-Length, X-API-Key, X-Metrics-Key, Range, If-Range, If-Modified-Since")
			w.Header().Set("Access-Control-Expose-Headers", "Content-Length, Content-Range, X-FileFlow-FileID, X-FileFlow-Original-Filename, X-FileFlow-SHA256")
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusOK)
			return
		}
		next.ServeHTTP(w, r)
	})
}
