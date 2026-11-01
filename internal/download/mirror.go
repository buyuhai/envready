// 本文件负责：拼接下载地址、给多镜像测速并排序。
// 边界：只做"问一下哪个镜像快"，不下载正文、不写磁盘。
//
// 为什么不用 HEAD 探测：相当多镜像/CDN 对 HEAD 返回 405 或直接 200 但不带
// Content-Length，甚至返回与 GET 不同的结果。参考实现的实测里就有这一类问题。
// 所以统一用 GET + Range: bytes=0-0 探活——它既便宜（只取 1 字节），
// 又能顺带确认服务端是否支持断点续传。

package download

import (
	"context"
	"errors"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// probeTimeout 是单个镜像的探测超时。探测只是"问一句"，不该拖太久。
const probeTimeout = 8 * time.Second

// joinURL 把镜像前缀与文件名拼成完整地址。
//
// 兼容 prefix 末尾有无斜杠、file 开头有无斜杠的情况——这类不一致
// 在真实清单里经常出现，拼错会得到 404。
func joinURL(prefix, file string) string {
	p := strings.TrimRight(strings.TrimSpace(prefix), "/")
	f := strings.TrimLeft(strings.TrimSpace(file), "/")
	return p + "/" + f
}

// mirrorLatency 是一个可用镜像及其探测耗时。
type mirrorLatency struct {
	url     string
	latency time.Duration
}

// probeMirrors 并发探测所有镜像，返回按"探测成功且耗时从短到长"排序的结果。
//
// 失败：没有任何镜像可用时返回空切片（调用方据此报 ErrAllMirrors）。
// 副作用：会向每个镜像发一个 Range: bytes=0-0 的 GET 请求。
func probeMirrors(ctx context.Context, client *http.Client, urls []string, headers map[string]string) []mirrorLatency {
	type result struct {
		m  mirrorLatency
		ok bool
	}
	ch := make(chan result, len(urls))
	var wg sync.WaitGroup

	for _, u := range urls {
		wg.Add(1)
		go func(u string) {
			defer wg.Done()
			lat, ok := probeOne(ctx, client, u, headers)
			ch <- result{m: mirrorLatency{url: u, latency: lat}, ok: ok}
		}(u)
	}
	wg.Wait()
	close(ch)

	var out []mirrorLatency
	for r := range ch {
		if r.ok {
			out = append(out, r.m)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].latency < out[j].latency })
	return out
}

// probeOne 探测单个地址，返回耗时与是否可用。
func probeOne(ctx context.Context, client *http.Client, url string, headers map[string]string) (time.Duration, bool) {
	pctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(pctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, false
	}
	req.Header.Set("Range", "bytes=0-0")
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return 0, false
	}
	defer func() {
		// 只读 1 字节，然后丢弃；不关闭会导致连接泄漏
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1))
		_ = resp.Body.Close()
	}()

	// 200 表示服务端忽略了 Range（仍可用，只是不支持续传）；206 表示支持续传
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		return 0, false
	}
	return time.Since(start), true
}

// ErrAllMirrors 表示所有镜像都不可用。
var ErrAllMirrors = errors.New("所有下载镜像均不可用")
