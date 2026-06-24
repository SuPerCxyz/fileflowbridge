package main

import (
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
)

// ==================== 简易 leveled logger ====================
//
// 通过环境变量 FFB_LOG_LEVEL 控制日志级别（DEBUG/INFO/WARN/ERROR），
// 所有调用最终走 stdlib log。已有代码中的 log.Printf 视为 INFO。
// 新增日志按需调用 logDebug / logInfo / logWarn / logError。

const (
	logLevelDebug = 0
	logLevelInfo  = 1
	logLevelWarn  = 2
	logLevelError = 3
)

var currentLogLevel atomic.Int32 // 默认 INFO

func setLogLevel(name string) {
	switch strings.ToUpper(strings.TrimSpace(name)) {
	case "DEBUG":
		currentLogLevel.Store(logLevelDebug)
	case "INFO", "":
		currentLogLevel.Store(logLevelInfo)
	case "WARN", "WARNING":
		currentLogLevel.Store(logLevelWarn)
	case "ERROR":
		currentLogLevel.Store(logLevelError)
	default:
		currentLogLevel.Store(logLevelInfo)
		log.Printf("⚠️ 未知日志级别 %q，已回落到 INFO", name)
	}
}

func logEnabled(level int32) bool { return currentLogLevel.Load() <= level }

func logDebug(format string, args ...interface{}) {
	if logEnabled(logLevelDebug) {
		log.Printf("[DEBUG] "+format, args...)
	}
}

func logInfo(format string, args ...interface{}) {
	if logEnabled(logLevelInfo) {
		log.Printf("[INFO] "+format, args...)
	}
}

func logWarn(format string, args ...interface{}) {
	if logEnabled(logLevelWarn) {
		log.Printf("[WARN] "+format, args...)
	}
}

func logError(format string, args ...interface{}) {
	if logEnabled(logLevelError) {
		log.Printf("[ERROR] "+format, args...)
	}
}

// setupLogging 根据环境变量初始化日志输出
func setupLogging() {
	logLevel := os.Getenv("FFB_LOG_LEVEL")
	if logLevel == "" {
		logLevel = "INFO"
	}
	setLogLevel(logLevel)

	logPath := os.Getenv("FFB_LOG_PATH")
	if logPath == "" {
		logPath = "fileflow_bridge.log"
	}

	if isRunningInContainer() {
		fmt.Println("🐳 检测到容器环境，日志仅输出到控制台")
		log.SetOutput(os.Stdout)
		return
	}

	logDir := filepath.Dir(logPath)
	if logDir != "" {
		os.MkdirAll(logDir, 0755)
	}

	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0666)
	if err != nil {
		log.SetOutput(os.Stdout)
		fmt.Printf("⚠️ 无法打开日志文件 %s: %v，仅输出到控制台\n", logPath, err)
		return
	}
	log.SetOutput(io.MultiWriter(os.Stdout, logFile))
	fmt.Printf("📝 日志文件: %s\n", logPath)
}
