package main

import (
	"crypto/rand"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/google/uuid"
)

// ==================== 环境变量辅助 ====================

func getEnvInt(key string, defaultVal int) int {
	if val := os.Getenv(key); val != "" {
		if i, err := strconv.Atoi(val); err == nil {
			return i
		}
	}
	return defaultVal
}

func getEnvInt64(key string, defaultVal int64) int64 {
	if val := os.Getenv(key); val != "" {
		if i, err := strconv.ParseInt(val, 10, 64); err == nil {
			return i
		}
	}
	return defaultVal
}

// ==================== HTTP / 主机名辅助 ====================

// getScheme 返回请求方案，优先尊重反向代理头
func getScheme(r *http.Request) string {
	if scheme := r.Header.Get("X-Forwarded-Proto"); scheme != "" {
		return scheme
	}
	if scheme := r.Header.Get("X-Forwarded-Scheme"); scheme != "" {
		return scheme
	}
	if r.TLS != nil {
		return "https"
	}
	return "http"
}

// getHost 返回去掉端口的主机名，兼容 IPv6 [::1]:8000
func getHost(r *http.Request) string {
	host := r.Host
	if host == "" {
		return host
	}
	if h, _, err := net.SplitHostPort(host); err == nil {
		return h
	}
	return host
}

// formatFileSize 将字节数转为人类可读
func formatFileSize(size int64) string {
	if size <= 0 {
		return "0 B"
	}

	const unit = 1024
	if size < unit {
		return fmt.Sprintf("%d B", size)
	}

	div, exp := int64(unit), 0
	for n := size / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}

	return fmt.Sprintf("%.2f %cB", float64(size)/float64(div), "KMGTPE"[exp])
}

// ==================== 浏览器检测 ====================

func isBrowserRequest(r *http.Request) bool {
	userAgent := strings.ToLower(r.UserAgent())
	acceptHeader := r.Header.Get("Accept")

	browserIndicators := []string{
		"mozilla/", "chrome/", "safari/", "firefox/", "edge/", "opera/",
	}
	isBrowser := false
	for _, indicator := range browserIndicators {
		if strings.Contains(userAgent, indicator) {
			isBrowser = true
			break
		}
	}

	acceptContainsStar := strings.Contains(acceptHeader, "*/*")

	commandLineIndicators := []string{
		"wget", "curl", "lwp-request", "libwww-perl",
		"python-urllib", "java", "okhttp",
	}
	isCommandLine := false
	for _, indicator := range commandLineIndicators {
		if strings.Contains(userAgent, indicator) {
			isCommandLine = true
			break
		}
	}

	return isBrowser && !isCommandLine && acceptContainsStar
}

// ==================== 容器环境检测 ====================

func isRunningInContainer() bool {
	if _, err := os.Stat("/.dockerenv"); err == nil {
		return true
	}

	if _, err := os.Stat("/proc/1/cgroup"); err == nil {
		if content, err := os.ReadFile("/proc/1/cgroup"); err == nil {
			cgroup := string(content)
			if strings.Contains(cgroup, "docker") || strings.Contains(cgroup, "kubepods") {
				return true
			}
		}
	}

	containerVars := []string{"KUBERNETES_SERVICE_HOST", "CONTAINER", "DOCKER_CONTAINER"}
	for _, envVar := range containerVars {
		if os.Getenv(envVar) != "" {
			return true
		}
	}

	return false
}

// ==================== Token 生成 ====================

// createNewID 在 6..32 范围内生成密码学安全 ID；超出范围回退到 UUID
func (ffb *FileFlowBridge) createNewID() string {
	if ffb.TokenLength < 6 || ffb.TokenLength > 32 {
		return uuid.New().String()
	}
	const charset = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	ret := make([]byte, ffb.TokenLength)
	for i := 0; i < ffb.TokenLength; i++ {
		num, _ := rand.Int(rand.Reader, big.NewInt(int64(len(charset))))
		ret[i] = charset[num.Int64()]
	}
	return string(ret)
}
