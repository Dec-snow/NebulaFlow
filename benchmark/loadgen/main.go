// NebulaFlow 压测客户端。
//
// 为什么不用 wrk：
//   - wrk 依赖 epoll/kqueue，Windows 上没有可用的原生构建；
//   - 更关键的是 wrk 只能测「HTTP 提交」这一段，而本项目的核心指标是
//     「提交吞吐 + 任务真正落地完成的端到端时延」，需要同时观测两段。
//
// 这个工具因此分两个阶段：
//  1. 提交阶段：C 个并发连接尽可能快地 POST /api/tasks，记录每请求时延与状态码；
//  2. 排水阶段：轮询数据库，等所有已提交任务进入终态，统计完成率与端到端时延。
//
// 用法：
//
//	go run ./benchmark/loadgen \
//	  -c 50 -n 3000 -workflow 1 -label "workers=20" \
//	  -dsn "postgres://nebula:nebula@localhost:5432/nebulaflow?sslmode=disable"
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

type options struct {
	baseURL     string
	username    string
	password    string
	workflowID  int64
	concurrency int
	requests    int
	warmup      int
	dsn         string
	label       string
	workers     int
	drainWait   time.Duration
	out         string
	timeout     time.Duration
}

// ---------- 结果结构 ----------

type latencyStats struct {
	Min  float64 `json:"min_ms"`
	Mean float64 `json:"mean_ms"`
	P50  float64 `json:"p50_ms"`
	P90  float64 `json:"p90_ms"`
	P95  float64 `json:"p95_ms"`
	P99  float64 `json:"p99_ms"`
	Max  float64 `json:"max_ms"`
}

type submitResult struct {
	Requests    int              `json:"requests"`
	OK          int64            `json:"ok"`
	Failed      int64            `json:"failed"`
	SuccessRate float64          `json:"success_rate"`
	WallSeconds float64          `json:"wall_seconds"`
	RPS         float64          `json:"rps"`
	StatusCodes map[string]int64 `json:"status_codes"`
	Latency     latencyStats     `json:"latency"`
	Errors      map[string]int64 `json:"errors,omitempty"`
}

type drainResult struct {
	Seconds      float64      `json:"seconds"`
	Succeeded    int64        `json:"succeeded"`
	Failed       int64        `json:"failed"`
	Pending      int64        `json:"pending"`
	CompletionRT float64      `json:"completion_rate"`
	TaskPerSec   float64      `json:"task_throughput_per_sec"`
	Latency      latencyStats `json:"end_to_end_latency"`
	Note         string       `json:"note,omitempty"`
}

type report struct {
	Label       string       `json:"label"`
	Workers     int          `json:"worker_count"`
	Concurrency int          `json:"concurrency"`
	WorkflowID  int64        `json:"workflow_id"`
	StartedAt   string       `json:"started_at"`
	Submit      submitResult `json:"submit"`
	Drain       drainResult  `json:"drain"`
}

func main() {
	var o options
	flag.StringVar(&o.baseURL, "url", "http://localhost:8080", "服务地址")
	flag.StringVar(&o.username, "user", "demo", "登录用户名")
	flag.StringVar(&o.password, "pass", "demo123456", "登录密码")
	flag.Int64Var(&o.workflowID, "workflow", 1, "要提交的工作流 ID")
	flag.IntVar(&o.concurrency, "c", 50, "并发连接数")
	flag.IntVar(&o.requests, "n", 3000, "提交请求总数")
	flag.IntVar(&o.warmup, "warmup", 50, "预热请求数（不计入统计）")
	flag.StringVar(&o.dsn, "dsn", "", "PostgreSQL DSN，用于统计任务落地（留空则跳过排水阶段）")
	flag.StringVar(&o.label, "label", "", "本轮标签，写入报告")
	flag.IntVar(&o.workers, "workers", 0, "本轮服务端 WORKER_COUNT，仅用于报告标注")
	flag.DurationVar(&o.drainWait, "drain-wait", 120*time.Second, "等待任务全部落地的上限")
	flag.DurationVar(&o.timeout, "timeout", 60*time.Second, "单请求超时")
	flag.StringVar(&o.out, "out", "", "把 JSON 报告写到该文件")
	flag.Parse()

	if o.label == "" {
		o.label = fmt.Sprintf("workers=%d/c=%d", o.workers, o.concurrency)
	}

	rep, err := run(o)
	if err != nil {
		fmt.Fprintf(os.Stderr, "压测失败: %v\n", err)
		os.Exit(1)
	}
	printReport(rep)

	if o.out != "" {
		b, _ := json.MarshalIndent(rep, "", "  ")
		if err := os.WriteFile(o.out, b, 0o644); err != nil {
			fmt.Fprintf(os.Stderr, "写报告失败: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("\nJSON 报告已写入 %s\n", o.out)
	}
}

func run(o options) (*report, error) {
	client := newClient(o.concurrency, o.timeout)

	token, err := ensureToken(client, o)
	if err != nil {
		return nil, fmt.Errorf("准备账号失败: %w", err)
	}

	wfID, err := ensureWorkflow(client, o, token)
	if err != nil {
		return nil, fmt.Errorf("准备工作流失败: %w", err)
	}
	o.workflowID = wfID

	// 预热：让连接池、Redis 连接、JIT 路径都热起来，避免把冷启动算进稳态
	if o.warmup > 0 {
		if _, _, err := submitBatch(client, o, token, o.warmup, 4); err != nil {
			return nil, fmt.Errorf("预热失败: %w", err)
		}
	}

	started := time.Now()
	ids, sub, err := submitBatch(client, o, token, o.requests, o.concurrency)
	if err != nil {
		return nil, err
	}
	submitWall := time.Since(started).Seconds()

	sub.WallSeconds = round(submitWall, 3)
	sub.RPS = round(float64(sub.Requests)/submitWall, 1)
	sub.SuccessRate = round(float64(sub.OK)/float64(sub.Requests)*100, 2)

	rep := &report{
		Label:       o.label,
		Workers:     o.workers,
		Concurrency: o.concurrency,
		WorkflowID:  o.workflowID,
		StartedAt:   started.Format(time.RFC3339),
		Submit:      sub,
	}

	if o.dsn == "" {
		rep.Drain = drainViaAPI(o, token, ids, time.Now())
	} else {
		rep.Drain = drain(o, ids, time.Now())
	}
	return rep, nil
}

// drainViaAPI 用 HTTP 接口统计任务落地情况。
// 内存模式（STORAGE_MODE=memory）下没有数据库可以旁路查询，
// 只能从被测系统自己的列表接口取状态——好处是这条路径对两种存储后端都成立。
func drainViaAPI(o options, token string, ids []int64, since time.Time) drainResult {
	if len(ids) == 0 {
		return drainResult{Note: "没有成功提交的任务 ID"}
	}
	c := &http.Client{Timeout: 20 * time.Second}
	deadline := time.Now().Add(o.drainWait)
	var last drainResult

	for {
		var succeeded, failed, pending int64
		var e2e []float64

		// 列表接口把 limit 上限钳在 100（超出范围会被静默重置为 20），必须翻页统计
		for offset := 0; ; offset += 100 {
			pageURL := fmt.Sprintf("%s/api/tasks?limit=100&offset=%d", o.baseURL, offset)
			resp, err := doJSON(c, http.MethodGet, pageURL, token, nil)
			if err != nil {
				return drainResult{Note: "查询任务列表失败: " + err.Error()}
			}
			raw, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				return drainResult{Note: fmt.Sprintf("查询任务列表 status=%d", resp.StatusCode)}
			}

			var payload struct {
				Tasks []struct {
					Status     string `json:"status"`
					CreatedAt  string `json:"created_at"`
					FinishedAt string `json:"finished_at"`
				} `json:"tasks"`
			}
			if err := json.Unmarshal(raw, &payload); err != nil {
				return drainResult{Note: "解析任务列表失败: " + err.Error()}
			}

			for _, t := range payload.Tasks {
				switch t.Status {
				case "succeeded":
					succeeded++
				case "failed", "cancelled":
					failed++
				default:
					pending++
				}
				if t.FinishedAt != "" && t.CreatedAt != "" {
					cs, e1 := time.Parse(time.RFC3339Nano, t.CreatedAt)
					fs, e2 := time.Parse(time.RFC3339Nano, t.FinishedAt)
					if e1 == nil && e2 == nil {
						e2e = append(e2e, float64(fs.Sub(cs).Microseconds())/1000.0)
					}
				}
			}
			if len(payload.Tasks) < 100 {
				break
			}
		}

		last.Succeeded, last.Failed, last.Pending = succeeded, failed, pending
		last.Latency = stats(e2e)
		if pending == 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}

	last.Seconds = round(time.Since(since).Seconds(), 3)
	total := last.Succeeded + last.Failed + last.Pending
	if total > 0 {
		last.CompletionRT = round(float64(last.Succeeded+last.Failed)/float64(total)*100, 2)
	}
	if last.Seconds > 0 {
		last.TaskPerSec = round(float64(last.Succeeded+last.Failed)/last.Seconds, 1)
	}
	if last.Pending > 0 {
		last.Note = fmt.Sprintf("等待超时，仍有 %d 个任务未进入终态", last.Pending)
	}
	return last
}

func newClient(concurrency int, timeout time.Duration) *http.Client {
	tr := &http.Transport{
		Proxy: nil,
		DialContext: (&net.Dialer{
			Timeout:   5 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		MaxIdleConns:        concurrency * 4,
		MaxIdleConnsPerHost: concurrency * 4,
		MaxConnsPerHost:     0,
		IdleConnTimeout:     90 * time.Second,
		DisableCompression:  true,
	}
	return &http.Client{Transport: tr, Timeout: timeout}
}

// doJSON 发一个 JSON 请求，返回原始响应。
func doJSON(c *http.Client, method, url, token string, body []byte) (*http.Response, error) {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, url, rdr)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return c.Do(req)
}

// ensureToken 先尝试登录；内存模式（STORAGE_MODE=memory）下没有 seed 数据，
// 登录必然失败，此时自动注册一个压测专用账号，让压测自举。
func ensureToken(c *http.Client, o options) (string, error) {
	if tok, err := login(c, o); err == nil {
		return tok, nil
	}
	body, _ := json.Marshal(map[string]string{
		"username": o.username,
		"email":    o.username + "@nebulaflow.dev",
		"password": o.password,
	})
	resp, err := doJSON(c, http.MethodPost, o.baseURL+"/api/auth/register", "", body)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("注册 status=%d body=%s", resp.StatusCode, truncate(string(raw), 200))
	}
	var out struct {
		Token string `json:"token"`
	}
	if json.Unmarshal(raw, &out) != nil || out.Token == "" {
		return "", fmt.Errorf("注册响应无 token: %s", truncate(string(raw), 200))
	}
	return out.Token, nil
}

// ensureWorkflow 返回一个可提交的任务目标工作流：
// 已有则复用，没有就按 seed 的同一拓扑（5 节点 DAG）现场创建一个。
func ensureWorkflow(c *http.Client, o options, token string) (int64, error) {
	if ids, err := listWorkflowIDs(c, o, token); err == nil && len(ids) > 0 {
		return ids[0], nil
	}

	kbID, err := ensureKnowledgeBase(c, o, token)
	if err != nil {
		return 0, err
	}

	payload, _ := json.Marshal(map[string]any{
		"name":        "压测工作流",
		"description": "benchmark: parser → rag → analyst → writer → output",
		"status":      "published",
		"nodes": []map[string]any{
			{"key": "parser", "type": "input", "x": 50, "y": 100,
				"config": map[string]any{"system": "你是简历解析器。"}},
			{"key": "rag", "type": "rag", "x": 50, "y": 260,
				"config": map[string]any{"knowledge_base_id": kbID}},
			{"key": "analyst", "type": "llm", "x": 320, "y": 180,
				"config": map[string]any{"model": "mock-chat", "system": "你是资深招聘分析师。",
					"prompt": "分析候选人与岗位的匹配度。", "max_retry": 2, "timeout_sec": 60}},
			{"key": "writer", "type": "llm", "x": 590, "y": 180,
				"config": map[string]any{"model": "mock-chat", "system": "你是技术写作助手。",
					"prompt": "整理成结构化报告。", "max_retry": 2, "timeout_sec": 60}},
			{"key": "output", "type": "output", "x": 860, "y": 180,
				"config": map[string]any{}},
		},
		"edges": []map[string]any{
			{"source": "parser", "target": "rag"},
			{"source": "parser", "target": "analyst"},
			{"source": "rag", "target": "analyst"},
			{"source": "analyst", "target": "writer"},
			{"source": "writer", "target": "output"},
		},
	})

	resp, err := doJSON(c, http.MethodPost, o.baseURL+"/api/workflows", token, payload)
	if err != nil {
		return 0, err
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("创建工作流 status=%d body=%s", resp.StatusCode, truncate(string(raw), 300))
	}

	// 不依赖创建接口的返回结构，重新列一次拿 ID
	ids, err := listWorkflowIDs(c, o, token)
	if err != nil || len(ids) == 0 {
		return 0, fmt.Errorf("创建后仍列不到工作流: %v", err)
	}
	return ids[0], nil
}

// ensureKnowledgeBase 保证存在一个已索引文档的知识库。
// RAG 节点在 knowledge_base_id 未配置时会直接失败，所以压测工作流必须绑定一个真实知识库；
// 语料刻意放大到几十个 chunk，这样检索链路的扫描代价才会显现（对应优化清单 P0-1）。
func ensureKnowledgeBase(c *http.Client, o options, token string) (int64, error) {
	if ids, err := listKBIDs(c, o, token); err == nil && len(ids) > 0 {
		return ids[0], nil
	}

	body, _ := json.Marshal(map[string]string{
		"name":        "压测知识库",
		"description": "benchmark corpus",
	})
	resp, err := doJSON(c, http.MethodPost, o.baseURL+"/api/knowledge-bases", token, body)
	if err != nil {
		return 0, err
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("创建知识库 status=%d body=%s", resp.StatusCode, truncate(string(raw), 200))
	}

	ids, err := listKBIDs(c, o, token)
	if err != nil || len(ids) == 0 {
		return 0, fmt.Errorf("创建后仍列不到知识库: %v", err)
	}
	kbID := ids[0]

	doc, _ := json.Marshal(map[string]string{
		"filename": "bench-corpus.md",
		"content":  benchCorpus(),
	})
	dresp, err := doJSON(c, http.MethodPost,
		fmt.Sprintf("%s/api/knowledge-bases/%d/documents", o.baseURL, kbID), token, doc)
	if err != nil {
		return 0, err
	}
	drawn, _ := io.ReadAll(dresp.Body)
	dresp.Body.Close()
	if dresp.StatusCode != http.StatusCreated && dresp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("上传文档 status=%d body=%s", dresp.StatusCode, truncate(string(drawn), 200))
	}
	return kbID, nil
}

func listKBIDs(c *http.Client, o options, token string) ([]int64, error) {
	resp, err := doJSON(c, http.MethodGet, o.baseURL+"/api/knowledge-bases", token, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("列知识库 status=%d", resp.StatusCode)
	}

	var arr []map[string]any
	if json.Unmarshal(raw, &arr) == nil {
		return idsOf(arr), nil
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(raw, &obj) == nil {
		for _, k := range []string{"knowledge_bases", "kbs", "items", "data", "list"} {
			if v, ok := obj[k]; ok {
				var a []map[string]any
				if json.Unmarshal(v, &a) == nil {
					return idsOf(a), nil
				}
			}
		}
	}
	return nil, nil
}

// benchCorpus 生成压测语料。内容与 seed 的岗位 JD 同源，
// 但重复放大到约 30KB，分块后产生数十个 chunk。
func benchCorpus() string {
	var b strings.Builder
	b.WriteString(jdSection)
	for i := 0; i < 24; i++ {
		fmt.Fprintf(&b, "\n\n## 补充章节 %d\n\n", i+1)
		b.WriteString(jdSection)
	}
	return b.String()
}

const jdSection = `### 岗位职责
1. 负责高并发 AI Agent 工作流平台的架构设计与核心模块开发，包括 DAG 任务调度、工作流编排与执行引擎。
2. 设计并实现 Worker Pool 与任务队列，支撑大规模并行任务的高吞吐执行与背压控制。
3. 建设 LLM Gateway 多模型接入层，实现模型故障自动切换与统一观测。
4. 参与 RAG 知识库检索链路建设，优化文档分块、向量化与检索召回质量。
5. 负责平台可观测性建设：指标采集、日志、追踪，保障系统稳定性。

### 任职要求
1. 精通 Go 语言，熟悉 goroutine 并发模型、channel 通信与 context 生命周期管理。
2. 熟悉 Docker、Kubernetes、CI/CD 流水线，有容器化部署与云原生实践经验。
3. 熟练使用 PostgreSQL、Redis，理解事务、索引与缓存一致性。
4. 了解主流 LLM API 与 RAG 技术栈，有实际落地经验者优先。
5. 有任务调度、工作流引擎或事件驱动架构设计经验者优先。`

func listWorkflowIDs(c *http.Client, o options, token string) ([]int64, error) {
	resp, err := doJSON(c, http.MethodGet, o.baseURL+"/api/workflows", token, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("列工作流 status=%d", resp.StatusCode)
	}

	var arr []map[string]any
	if json.Unmarshal(raw, &arr) == nil {
		return idsOf(arr), nil
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(raw, &obj) == nil {
		for _, k := range []string{"workflows", "items", "data", "list"} {
			if v, ok := obj[k]; ok {
				var a []map[string]any
				if json.Unmarshal(v, &a) == nil {
					return idsOf(a), nil
				}
			}
		}
	}
	return nil, nil
}

func idsOf(items []map[string]any) []int64 {
	var ids []int64
	for _, it := range items {
		if v, ok := it["id"].(float64); ok && v > 0 {
			ids = append(ids, int64(v))
		}
	}
	return ids
}

func login(c *http.Client, o options) (string, error) {
	body, _ := json.Marshal(map[string]string{"username": o.username, "password": o.password})
	req, _ := http.NewRequest(http.MethodPost, o.baseURL+"/api/auth/login", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("status=%d body=%s", resp.StatusCode, string(raw))
	}
	var out struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", err
	}
	if out.Token == "" {
		return "", fmt.Errorf("响应中没有 token: %s", string(raw))
	}
	return out.Token, nil
}

// submitBatch 用 concurrency 个 goroutine 打满 requests 个提交请求。
// 每个 goroutine 各自收集时延，最后合并——避免共享切片加锁成为瓶颈。
func submitBatch(c *http.Client, o options, token string, requests, concurrency int) ([]int64, submitResult, error) {
	if requests <= 0 {
		return nil, submitResult{}, nil
	}
	if concurrency <= 0 {
		concurrency = 1
	}
	if concurrency > requests {
		concurrency = requests
	}

	var (
		next     atomic.Int64
		okCount  atomic.Int64
		failCnt  atomic.Int64
		idsMu    sync.Mutex
		ids      = make([]int64, 0, requests)
		statusMu sync.Mutex
		statuses = map[string]int64{}
		errMu    sync.Mutex
		errs     = map[string]int64{}
		lats     = make([][]float64, concurrency)
	)

	payload, _ := json.Marshal(map[string]any{
		"workflow_id": o.workflowID,
		"input":       "压测输入：验证 DAG 调度在高并发下的稳定性与吞吐",
	})

	var wg sync.WaitGroup
	for w := 0; w < concurrency; w++ {
		wg.Add(1)
		go func(slot int) {
			defer wg.Done()
			local := make([]float64, 0, requests/concurrency+1)
			for {
				i := next.Add(1)
				if int(i) > requests {
					break
				}
				req, err := http.NewRequest(http.MethodPost, o.baseURL+"/api/tasks", bytes.NewReader(payload))
				if err != nil {
					failCnt.Add(1)
					continue
				}
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("Authorization", "Bearer "+token)

				t0 := time.Now()
				resp, err := c.Do(req)
				elapsed := float64(time.Since(t0).Microseconds()) / 1000.0
				local = append(local, elapsed)

				if err != nil {
					failCnt.Add(1)
					errMu.Lock()
					errs[shortErr(err)]++
					errMu.Unlock()
					continue
				}
				raw, _ := io.ReadAll(resp.Body)
				resp.Body.Close()

				statusMu.Lock()
				statuses[fmt.Sprintf("%d", resp.StatusCode)]++
				statusMu.Unlock()

				if resp.StatusCode >= 200 && resp.StatusCode < 300 {
					okCount.Add(1)
					var created struct {
						ID int64 `json:"id"`
					}
					if json.Unmarshal(raw, &created) == nil && created.ID > 0 {
						idsMu.Lock()
						ids = append(ids, created.ID)
						idsMu.Unlock()
					}
				} else {
					failCnt.Add(1)
					errMu.Lock()
					errs[fmt.Sprintf("HTTP %d: %s", resp.StatusCode, truncate(string(raw), 120))]++
					errMu.Unlock()
				}
			}
			lats[slot] = local
		}(w)
	}
	wg.Wait()

	all := make([]float64, 0, requests)
	for _, l := range lats {
		all = append(all, l...)
	}
	sort.Slice(ids, func(a, b int) bool { return ids[a] < ids[b] })

	res := submitResult{
		Requests:    requests,
		OK:          okCount.Load(),
		Failed:      failCnt.Load(),
		StatusCodes: statuses,
		Errors:      errs,
		Latency:     stats(all),
	}
	return ids, res, nil
}

// drain 轮询数据库，等待所有任务进入终态。
func drain(o options, ids []int64, since time.Time) drainResult {
	if len(ids) == 0 {
		return drainResult{Note: "没有成功提交的任务 ID"}
	}
	ctx, cancel := context.WithTimeout(context.Background(), o.drainWait+30*time.Second)
	defer cancel()

	pool, err := pgxpool.New(ctx, o.dsn)
	if err != nil {
		return drainResult{Note: "连接数据库失败: " + err.Error()}
	}
	defer pool.Close()

	deadline := time.Now().Add(o.drainWait)
	var last drainResult
	for {
		var succeeded, failed, pending int64
		err := pool.QueryRow(ctx, `
			SELECT
			  count(*) FILTER (WHERE status = 'succeeded'),
			  count(*) FILTER (WHERE status IN ('failed','cancelled')),
			  count(*) FILTER (WHERE status IN ('pending','running'))
			FROM tasks WHERE id = ANY($1)`, ids).Scan(&succeeded, &failed, &pending)
		if err != nil {
			return drainResult{Note: "查询任务状态失败: " + err.Error()}
		}

		last.Succeeded, last.Failed, last.Pending = succeeded, failed, pending
		if pending == 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}

	last.Seconds = round(time.Since(since).Seconds(), 3)
	total := last.Succeeded + last.Failed + last.Pending
	if total > 0 {
		last.CompletionRT = round(float64(last.Succeeded+last.Failed)/float64(total)*100, 2)
	}
	if last.Seconds > 0 {
		last.TaskPerSec = round(float64(last.Succeeded+last.Failed)/last.Seconds, 1)
	}

	// 端到端时延：created_at → finished_at
	rows, err := pool.Query(ctx, `
		SELECT EXTRACT(EPOCH FROM (finished_at - created_at)) * 1000
		FROM tasks
		WHERE id = ANY($1) AND finished_at IS NOT NULL`, ids)
	if err == nil {
		var d []float64
		for rows.Next() {
			var ms float64
			if rows.Scan(&ms) == nil {
				d = append(d, ms)
			}
		}
		rows.Close()
		last.Latency = stats(d)
	}
	if last.Pending > 0 {
		last.Note = fmt.Sprintf("等待超时，仍有 %d 个任务未进入终态", last.Pending)
	}
	return last
}

// ---------- 统计 ----------

func stats(d []float64) latencyStats {
	if len(d) == 0 {
		return latencyStats{}
	}
	sort.Float64s(d)
	sum := 0.0
	for _, v := range d {
		sum += v
	}
	return latencyStats{
		Min:  round(d[0], 2),
		Mean: round(sum/float64(len(d)), 2),
		P50:  round(pct(d, 50), 2),
		P90:  round(pct(d, 90), 2),
		P95:  round(pct(d, 95), 2),
		P99:  round(pct(d, 99), 2),
		Max:  round(d[len(d)-1], 2),
	}
}

func pct(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(math.Ceil(p/100*float64(len(sorted)))) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

func round(v float64, digits int) float64 {
	f := math.Pow10(digits)
	return math.Round(v*f) / f
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "..."
}

func shortErr(err error) string {
	s := err.Error()
	if len(s) > 100 {
		s = s[:100]
	}
	return s
}

func printReport(r *report) {
	fmt.Printf("\n================ %s ================\n", r.Label)
	fmt.Printf("服务端 worker 数: %d   并发: %d   工作流: %d\n\n", r.Workers, r.Concurrency, r.WorkflowID)

	s := r.Submit
	fmt.Println("【提交阶段】")
	fmt.Printf("  请求 %d  成功 %d  失败 %d  成功率 %.2f%%\n", s.Requests, s.OK, s.Failed, s.SuccessRate)
	fmt.Printf("  耗时 %.3fs   吞吐 %.1f req/s\n", s.WallSeconds, s.RPS)
	fmt.Printf("  时延(ms) min %.2f  mean %.2f  p50 %.2f  p90 %.2f  p95 %.2f  p99 %.2f  max %.2f\n",
		s.Latency.Min, s.Latency.Mean, s.Latency.P50, s.Latency.P90, s.Latency.P95, s.Latency.P99, s.Latency.Max)
	if len(s.StatusCodes) > 0 {
		fmt.Print("  状态码: ")
		for k, v := range s.StatusCodes {
			fmt.Printf("%s=%d ", k, v)
		}
		fmt.Println()
	}
	if len(s.Errors) > 0 {
		fmt.Println("  错误明细:")
		for k, v := range s.Errors {
			fmt.Printf("    [%d] %s\n", v, k)
		}
	}

	d := r.Drain
	fmt.Println("\n【任务落地（排水）阶段】")
	if d.Note != "" {
		fmt.Printf("  %s\n", d.Note)
	}
	fmt.Printf("  耗时 %.3fs   成功 %d  失败 %d  未完成 %d   落地率 %.2f%%\n",
		d.Seconds, d.Succeeded, d.Failed, d.Pending, d.CompletionRT)
	fmt.Printf("  任务吞吐 %.1f tasks/s\n", d.TaskPerSec)
	fmt.Printf("  端到端时延(ms) min %.2f  mean %.2f  p50 %.2f  p90 %.2f  p95 %.2f  p99 %.2f  max %.2f\n",
		d.Latency.Min, d.Latency.Mean, d.Latency.P50, d.Latency.P90, d.Latency.P95, d.Latency.P99, d.Latency.Max)
	fmt.Println("==================================================")
}
