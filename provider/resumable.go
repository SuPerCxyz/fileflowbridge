package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sync"
	"time"
)

// ==================== Resumable 上传 (provider 侧) ====================
//
// 流程：
//   1) RegisterFile(...) 拿到 chunk_size / total_chunks / chunk_upload_url / chunk_status_url
//   2) UploadChunked() 先调 chunk_status_url 看哪些已经发过（断点续传）
//   3) 用 UploadConc 个 goroutine 并发 PUT 缺失 chunk，每个 chunk 最多重试 MaxRetries 次

type chunkStatusResp struct {
	Resumable     bool  `json:"resumable"`
	TotalChunks   int   `json:"total_chunks"`
	ChunkSize     int64 `json:"chunk_size"`
	Size          int64 `json:"size"`
	ReceivedBytes int64 `json:"received_bytes"`
	MissingChunks []int `json:"missing_chunks"`
	UploadReady   bool  `json:"upload_ready"`
}

// UploadChunked 通过 PUT chunk 上传整个文件
func (f *FlowProvider) UploadChunked() error {
	if !f.Resumable || f.chunkUploadURL == "" {
		return fmt.Errorf("provider 未启用 resumable 或服务器未返回 chunk 上传地址")
	}
	if f.UploadConc <= 0 {
		f.UploadConc = 4
	}
	if f.MaxRetries <= 0 {
		f.MaxRetries = 5
	}

	// 查询当前已收到的 chunk
	missing, err := f.queryChunkStatus()
	if err != nil {
		return fmt.Errorf("查询 chunk 状态失败: %w", err)
	}
	if len(missing) == 0 {
		fmt.Println("✅ 所有 chunk 已在服务器，无需重发")
		return nil
	}

	fmt.Printf("🚀 开始上传 %d 个 chunk（并发 %d，重试 %d）\n",
		len(missing), f.UploadConc, f.MaxRetries)

	jobs := make(chan int, len(missing))
	for _, idx := range missing {
		jobs <- idx
	}
	close(jobs)

	var (
		wg        sync.WaitGroup
		errOnce   sync.Once
		firstErr  error
		uploaded  int64
		uploadedM sync.Mutex
	)

	totalBytes := f.FileInfo.Size
	startTime := time.Now()

	for i := 0; i < f.UploadConc; i++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			for idx := range jobs {
				if firstErr != nil {
					return
				}
				n, err := f.uploadOneChunk(idx)
				if err != nil {
					errOnce.Do(func() { firstErr = err })
					return
				}
				uploadedM.Lock()
				uploaded += n
				done := uploaded
				uploadedM.Unlock()
				elapsed := time.Since(startTime).Seconds()
				if elapsed > 0 {
					speed := float64(done) / elapsed / 1024 / 1024
					fmt.Printf("\r📤 进度: %.2f / %.2f MiB (%.2f MiB/s)   ",
						float64(done)/(1024*1024),
						float64(totalBytes)/(1024*1024),
						speed)
				}
			}
		}(i)
	}
	wg.Wait()
	fmt.Println()

	if firstErr != nil {
		return firstErr
	}

	// 最后再调一次 status 确认 upload_ready=true
	resp, err := f.queryChunkStatusFull()
	if err != nil {
		return fmt.Errorf("最终状态校验失败: %w", err)
	}
	if !resp.UploadReady {
		return fmt.Errorf("服务器未确认上传完成；missing=%v", resp.MissingChunks)
	}
	fmt.Println("🎉 Resumable 上传完成")
	return nil
}

func (f *FlowProvider) queryChunkStatus() ([]int, error) {
	resp, err := f.queryChunkStatusFull()
	if err != nil {
		return nil, err
	}
	return resp.MissingChunks, nil
}

func (f *FlowProvider) queryChunkStatusFull() (*chunkStatusResp, error) {
	req, err := http.NewRequest("GET", f.chunkStatusURL, nil)
	if err != nil {
		return nil, err
	}
	if f.APIKey != "" {
		req.Header.Set("X-API-Key", f.APIKey)
	}
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("status=%d body=%s", resp.StatusCode, string(body))
	}
	var out chunkStatusResp
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return &out, nil
}

// uploadOneChunk 上传单个 chunk，自动重试
func (f *FlowProvider) uploadOneChunk(index int) (int64, error) {
	chunkSize := f.actualChunkSize
	offset := int64(index) * chunkSize
	expected := chunkSize
	if index == f.totalChunks-1 {
		expected = f.FileInfo.Size - offset
	}

	var lastErr error
	for attempt := 1; attempt <= f.MaxRetries; attempt++ {
		n, err := f.uploadOneChunkOnce(index, offset, expected)
		if err == nil {
			return n, nil
		}
		lastErr = err
		// 指数退避，最长 8 秒
		backoff := time.Duration(1<<attempt) * 200 * time.Millisecond
		if backoff > 8*time.Second {
			backoff = 8 * time.Second
		}
		fmt.Printf("\n⚠️ chunk %d 第 %d 次失败: %v；%.2fs 后重试\n",
			index, attempt, err, backoff.Seconds())
		time.Sleep(backoff)
	}
	return 0, fmt.Errorf("chunk %d 重试 %d 次仍失败: %w", index, f.MaxRetries, lastErr)
}

func (f *FlowProvider) uploadOneChunkOnce(index int, offset, expected int64) (int64, error) {
	file, err := os.Open(f.FileInfo.Path)
	if err != nil {
		return 0, err
	}
	defer file.Close()

	if _, err := file.Seek(offset, io.SeekStart); err != nil {
		return 0, err
	}
	buf := make([]byte, expected)
	if _, err := io.ReadFull(file, buf); err != nil {
		return 0, fmt.Errorf("读取 chunk %d 失败: %w", index, err)
	}

	url := fmt.Sprintf("%s?index=%d", f.chunkUploadURL, index)
	req, err := http.NewRequest("PUT", url, bytes.NewReader(buf))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	req.ContentLength = expected
	if f.APIKey != "" {
		req.Header.Set("X-API-Key", f.APIKey)
	}

	client := &http.Client{Timeout: 5 * time.Minute}
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return 0, fmt.Errorf("status=%d body=%s", resp.StatusCode, string(body))
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return expected, nil
}
