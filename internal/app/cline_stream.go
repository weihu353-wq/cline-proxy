package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"sync"
	"time"

	"cline-go-proxy/internal/kit"
)

// ============ cline "必须流式" 模型自学习 ============
//
// 背景：cline 的免费模型列表接口（/ai/cline/recommended-models）每个条目只有
// id/name/description/tags，没有任何"该模型是否必须流式调用"的信号。同步层只能
// 按 id 命名约定推断：含 ":" 的 id（如 poolside/laguna-s-2.1:free）是 provider
// 自带的免费变体，普通非流式调用即可；不含 ":" 的（cline-free/*、z-ai/*）强制
// 上游流式（见 models.go syncRecommendedModels / proxy.go modelNeedsStream）。
//
// 约定猜错时上游会明确拒绝：HTTP 500 + {"error":"empty response content"}。
// 机制：非流式的 cline 请求收到该错误 → 说明该模型其实必须流式 → 记下它
//（DATA_DIR/.cline-stream-required.json，跨重启保留）→ 本次请求立即改用
// stream=true 重试（客户端全程无感：网关把上游 SSE 聚合回普通 JSON 返回）→
// 后续请求由 modelNeedsStream 直接命中，不再有重试开销。
//
// 单向学习：命名约定唯一会出错的方向是"漏判"（该流式却没强制），反向
//（多判）只是让网关多做一次流式聚合，对客户端透明、无副作用，不值得学。

// clineStreamFile 学习结果持久化路径。
func clineStreamFile() string {
	return kit.ResolveDataPath(".cline-stream-required.json")
}

var (
	clineStreamMu      sync.RWMutex
	clineStreamLearned map[string]string // model id -> 学习时间(RFC3339)
)

// clineEmptyStreamErr 响应是否呈"该模型必须流式"的指纹：HTTP 500 且 body 含
// "empty response content"。只认这一条精确特征——它是 cline 对非流式调用这类
// 模型的固定答复；其他 5xx（过载、瞬时故障）绝不触发学习。
func clineEmptyStreamErr(status int, body []byte) bool {
	if status != http.StatusInternalServerError {
		return false
	}
	return bytes.Contains(bytes.ToLower(body), []byte("empty response content"))
}

// clineAPIError cline 上游的非 200 答复。callClineAPI 对非 200 会读尽并关闭响应体
// 后返回 error，调用方拿不到 *http.Response；但"该模型必须流式"的判定只需要状态码
// + 错误体，用类型化错误把它们带出来即可，无需改动"非 200 返回 nil 响应"的既有约定。
type clineAPIError struct {
	Status         int
	Body           string
	RetryAfter     string
	RateLimitScope clineRateLimitScope
}

func (e *clineAPIError) Error() string {
	return fmt.Sprintf("API %d: %s", e.Status, e.Body)
}

// clineStreamRequired 该模型是否已被学习为"必须流式"。
func clineStreamRequired(modelID string) bool {
	clineStreamMu.RLock()
	defer clineStreamMu.RUnlock()
	_, ok := clineStreamLearned[modelID]
	return ok
}

// learnClineStreamRequired 记录并持久化"该模型必须流式"。重复学习无副作用。
func learnClineStreamRequired(modelID string) {
	if modelID == "" {
		return
	}
	clineStreamMu.Lock()
	if clineStreamLearned == nil {
		clineStreamLearned = map[string]string{}
	}
	if _, ok := clineStreamLearned[modelID]; ok {
		clineStreamMu.Unlock()
		return
	}
	clineStreamLearned[modelID] = time.Now().Format(time.RFC3339)
	// 持锁只做内存改动：写盘是慢 I/O，快照出去在锁外做，避免阻塞并发请求的
	// modelNeedsStream 读路径。
	snapshot := make(map[string]string, len(clineStreamLearned))
	for k, v := range clineStreamLearned {
		snapshot[k] = v
	}
	clineStreamMu.Unlock()

	log.Printf("cline stream learned: model=%s requires stream=true (persisted)", modelID)
	saveClineStream(snapshot)
}

// loadClineStreamLearned 启动时恢复学习结果。
func loadClineStreamLearned() {
	data, err := os.ReadFile(clineStreamFile())
	if err != nil || len(data) == 0 {
		return
	}
	var learned map[string]string
	if err := json.Unmarshal(data, &learned); err != nil {
		log.Printf("cline stream learn parse failed, ignoring: %v", err)
		return
	}
	clineStreamMu.Lock()
	clineStreamLearned = learned
	n := len(learned)
	clineStreamMu.Unlock()
	if n > 0 {
		log.Printf("cline stream learn loaded: %d model(s) require streaming", n)
	}
}

// saveClineStream 原子写盘（入参是不持锁的快照）。写失败只记日志不阻断请求——
// 学习结果是优化项，丢失只会让下一次非流式请求再付一次重试代价。
func saveClineStream(learned map[string]string) {
	data, err := json.MarshalIndent(learned, "", "  ")
	if err != nil {
		return
	}
	tmp := clineStreamFile() + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		log.Printf("cline stream learn save failed: %v", err)
		return
	}
	if err := os.Rename(tmp, clineStreamFile()); err != nil {
		log.Printf("cline stream learn save failed (rename): %v", err)
	}
}

// clineRawCallFn 是单次上游调用入口。生产路径由有界账号故障转移包装它；
// 测试可替换该变量，验证不会在已开始输出后重放整个请求。
var clineRawCallFn = callClineAPI

// clineCallFn 是调用方使用的入口。它仅在上游尚未返回任何字节、且可明确识别
// 为账号级额度限制时尝试另一活跃账号；共享供应商限流不会被误当成换号信号。
var clineCallFn = callClineWithAccountFailover

// callClineAutoStream 包住 callClineAPI 兜住"命名约定猜错"的情况：非流式请求若
// 被上游以 500 empty response content 拒绝，就学习该模型并立即改用流式重试。
// 返回值 streamed 告知调用方"这份响应体是 SSE，必须走聚合路径"——即便客户端
// 原本要的是非流式 JSON。
//
// 只有非流式请求才探测：客户端本来就要流式时无需任何判断。
func callClineAutoStream(ctx context.Context, params map[string]any, stream, useProxies bool) (*http.Response, *Account, bool, error) {
	resp, acc, err := clineCallFn(ctx, params, stream, useProxies)
	// 只看非流式请求的失败：流式请求不会被这条约定拒绝，成功响应（resp != nil）照旧直通。
	if err == nil || stream || resp != nil {
		return resp, acc, false, err
	}
	// callClineAPI 对非 200 返回的是 nil 响应 + clineAPIError；指纹只在其中。
	var apiErr *clineAPIError
	if !errors.As(err, &apiErr) || !clineEmptyStreamErr(apiErr.Status, []byte(apiErr.Body)) {
		return resp, acc, false, err
	}

	model, _ := params["model"].(string)
	model = normalizeRequestModel(model)
	log.Printf("cline model %s rejected non-stream call (500 empty response): learning + retrying streamed", model)
	learnClineStreamRequired(model)

	resp2, acc2, err2 := clineCallFn(ctx, params, true, useProxies)
	if err2 != nil {
		// 重试也失败：最后一次失败更接近当前可操作状态（例如 429 和
		// Retry-After），不能用首次 empty-response 错误覆盖它。
		return nil, acc2, false, err2
	}
	if acc2 != nil {
		acc = acc2
	}
	return resp2, acc, true, nil
}
