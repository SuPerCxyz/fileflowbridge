package main

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// ==================== CLI 下载页 ====================
//
// 通过 GitHub Release 提供 fileflowprovider 多平台二进制。
// 为避免浏览器跨域与 GitHub API 速率限制，bridge 侧代理最新 release 查询，
// 并提供静态 /cli 页面（UA 自动检测平台）。
//
// 路由：
//   - GET /cli                      -> CLI 下载页（static/cli.html）
//   - GET /cli/releases/latest      -> 代理 GitHub API，返回精简资产列表
//
// 配置：
//   - FFB_GITHUB_REPO / --github-repo：GitHub 仓库，格式 "owner/repo"，
//     默认 "SuPerCxyz/fileflowbridge"

const defaultGitHubRepo = "SuPerCxyz/fileflowbridge"

// githubRepoFor 返回用于查询 release 的 owner/repo
func (ffb *FileFlowBridge) githubRepoFor() string {
	if r := strings.TrimSpace(ffb.GitHubRepo); r != "" {
		return r
	}
	return defaultGitHubRepo
}

// handleCLIPage 返回 CLI 下载页
func (ffb *FileFlowBridge) handleCLIPage(w http.ResponseWriter, r *http.Request) {
	if _, err := os.Stat("./static/cli.html"); err == nil {
		http.ServeFile(w, r, "./static/cli.html")
		return
	}
	http.Error(w, "CLI 下载页未启用", http.StatusNotFound)
}

// ghAsset 精简后的资产信息
type ghAsset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
	Size               int64  `json:"size"`
	ContentType        string `json:"content_type"`
}

// ghReleaseResponse 精简后的 release 信息
type ghReleaseResponse struct {
	TagName     string    `json:"tag_name"`
	Name        string    `json:"name"`
	PublishedAt string    `json:"published_at"`
	Assets      []ghAsset `json:"assets"`
}

// handleCLILatestRelease 代理 GitHub API /releases/latest，精简返回
//
// 使用服务端请求避免浏览器直接调用 GitHub API 受 CORS 与速率限制影响。
// GitHub API 文档：https://docs.github.com/en/rest/releases/releases#get-the-latest-release
func (ffb *FileFlowBridge) handleCLILatestRelease(w http.ResponseWriter, r *http.Request) {
	repo := ffb.githubRepoFor()
	url := "https://api.github.com/repos/" + repo + "/releases/latest"

	client := &http.Client{Timeout: 10 * time.Second}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, url, nil)
	if err != nil {
		logWarn("构造 GitHub API 请求失败: %v", err)
		http.Error(w, "内部服务器错误", http.StatusInternalServerError)
		return
	}
	// 匿名调用受 60 req/h 限制；bridge 单实例足够。
	// 如需更高配额可配置 FFB_GITHUB_TOKEN。
	if tok := strings.TrimSpace(ffb.GitHubToken); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := client.Do(req)
	if err != nil {
		logWarn("请求 GitHub API 失败: %v", err)
		http.Error(w, "无法获取 GitHub Release", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		logWarn("GitHub API 返回 %d: %s", resp.StatusCode, string(body))
		http.Error(w, "GitHub Release 不可用", resp.StatusCode)
		return
	}

	var raw struct {
		TagName     string `json:"tag_name"`
		Name        string `json:"name"`
		PublishedAt string `json:"published_at"`
		Assets      []struct {
			Name               string `json:"name"`
			BrowserDownloadURL string `json:"browser_download_url"`
			Size               int64  `json:"size"`
			ContentType        string `json:"content_type"`
		} `json:"assets"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		logWarn("解析 GitHub API 响应失败: %v", err)
		http.Error(w, "上游响应解析失败", http.StatusBadGateway)
		return
	}

	out := ghReleaseResponse{
		TagName:     raw.TagName,
		Name:        raw.Name,
		PublishedAt: raw.PublishedAt,
		Assets:      make([]ghAsset, 0, len(raw.Assets)),
	}
	for _, a := range raw.Assets {
		out.Assets = append(out.Assets, ghAsset{
			Name:               a.Name,
			BrowserDownloadURL: a.BrowserDownloadURL,
			Size:               a.Size,
			ContentType:        a.ContentType,
		})
	}

	w.Header().Set("Content-Type", "application/json")
	// 缓存 5 分钟，减少 GitHub API 配额消耗
	w.Header().Set("Cache-Control", "public, max-age=300")
	_ = json.NewEncoder(w).Encode(out)
}
