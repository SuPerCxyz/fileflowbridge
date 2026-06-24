package main

import (
	"fmt"
	"html"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
)

// serveDownloadPage 浏览器访问 /download/{token}[/{filename}] 时返回的中间页。
//
// 优先使用 ./static/download.html 模板，可用占位符：
//   - {{FILENAME}}, {{FILESIZE_HUMAN}}, {{FILESIZE_RAW}},
//     {{ORIGINAL_FILENAME}}, {{TOKEN}}
func (ffb *FileFlowBridge) serveDownloadPage(w http.ResponseWriter, r *http.Request, authToken string, metadata *FileMetadata) {
	templatePath := "./static/download.html"
	if _, err := os.Stat(templatePath); os.IsNotExist(err) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprintf(w, defaultDownloadPageHTML,
			html.EscapeString(metadata.OriginalFilename),
			float64(metadata.Size)/(1024*1024),
			authToken,
			authToken,
			url.PathEscape(metadata.OriginalFilename),
			authToken,
			url.PathEscape(metadata.OriginalFilename))
		return
	}

	content, err := os.ReadFile(templatePath)
	if err != nil {
		logWarn("读取下载页面模板失败: %v", err)
		http.Error(w, "内部服务器错误", http.StatusInternalServerError)
		return
	}

	templateContent := string(content)
	templateContent = strings.ReplaceAll(templateContent, "{{FILENAME}}", url.PathEscape(metadata.OriginalFilename))
	templateContent = strings.ReplaceAll(templateContent, "{{FILESIZE_HUMAN}}", formatFileSize(metadata.Size))
	templateContent = strings.ReplaceAll(templateContent, "{{FILESIZE_RAW}}", strconv.FormatInt(metadata.Size, 10))
	templateContent = strings.ReplaceAll(templateContent, "{{ORIGINAL_FILENAME}}", html.EscapeString(metadata.OriginalFilename))
	templateContent = strings.ReplaceAll(templateContent, "{{TOKEN}}", authToken)

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, templateContent)
}

const defaultDownloadPageHTML = `<!DOCTYPE html>
<html>
<head>
	<title>文件下载 - FileFlow Bridge</title>
	<meta charset="utf-8">
	<style>
		body { font-family: Arial, sans-serif; text-align: center; padding: 50px; background-color: #f5f5f5; }
		.container { background: white; padding: 30px; border-radius: 10px; box-shadow: 0 2px 10px rgba(0,0,0,0.1); display: inline-block; }
		button { background: #4CAF50; color: white; padding: 15px 32px; text-align: center; text-decoration: none; display: inline-block; font-size: 16px; margin: 10px; cursor: pointer; border: none; border-radius: 5px; }
		button:hover { background: #45a049; }
		.info { margin: 20px 0; }
	</style>
</head>
<body>
	<div class="container">
		<h1>📥 文件下载</h1>
		<div class="info">
			<p><strong>文件名:</strong> %s</p>
			<p><strong>文件大小:</strong> %.2f MB</p>
			<p><strong>文件ID:</strong> %s</p>
		</div>
		<p>点击下方按钮开始下载:</p>
		<a href="/download/%s/%s" download><button>点击下载</button></a>
		<br>
		<a href="/download/%s/%s">直接下载链接</a>
	</div>
</body>
</html>`
