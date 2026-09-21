package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
)

// ---------- 与 loadgen 输出对齐的结果结构 ----------

type latencyStats struct {
	Min  float64 `json:"min_ms"`
	Mean float64 `json:"mean_ms"`
	P50  float64 `json:"p50_ms"`
	P90  float64 `json:"p90_ms"`
	P95  float64 `json:"p95_ms"`
	P99  float64 `json:"p99_ms"`
	Max  float64 `json:"max_ms"`
}

type report struct {
	Label       string `json:"label"`
	Workers     int    `json:"worker_count"`
	Consumers   int    `json:"consumer_count"` // 0 = 跟随 WORKER_COUNT
	Concurrency int    `json:"concurrency"`
	WorkflowID  int64  `json:"workflow_id"`
	StartedAt   string `json:"started_at"`
	Submit      struct {
		Requests    int              `json:"requests"`
		OK          int64            `json:"ok"`
		Failed      int64            `json:"failed"`
		SuccessRate float64          `json:"success_rate"`
		WallSeconds float64          `json:"wall_seconds"`
		RPS         float64          `json:"rps"`
		StatusCodes map[string]int64 `json:"status_codes"`
		Latency     latencyStats     `json:"latency"`
	} `json:"submit"`
	Drain struct {
		Seconds      float64      `json:"seconds"`
		Succeeded    int64        `json:"succeeded"`
		Failed       int64        `json:"failed"`
		Pending      int64        `json:"pending"`
		CompletionRT float64      `json:"completion_rate"`
		TaskPerSec   float64      `json:"task_throughput_per_sec"`
		Latency      latencyStats `json:"end_to_end_latency"`
		Note         string       `json:"note,omitempty"`
	} `json:"drain"`
}

// ---------- 环境重置 ----------

// flushRedis 清空当前 Redis 实例。
// 用 FLUSHALL 而不是删指定 key：消费组、PEL、DLQ、延迟 ZSET 分散在多个 key 上，
// 逐个删容易漏，压测要的是确定性的干净起点。
func flushRedis(addr string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	rdb := redis.NewClient(&redis.Options{Addr: addr})
	defer rdb.Close()
	if err := rdb.Ping(ctx).Err(); err != nil {
		return err
	}
	return rdb.FlushAll(ctx).Err()
}

// truncateTasks 清空任务相关表，保留用户 / 工作流 / 知识库 / Provider。
func truncateTasks(dsn string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return err
	}
	defer pool.Close()
	_, err = pool.Exec(ctx,
		`TRUNCATE tasks, task_nodes, task_logs, usage_records RESTART IDENTITY CASCADE`)
	return err
}

// ---------- 汇总输出 ----------

func printMatrix(rs []*report) {
	fmt.Println("\n\n================ 压测矩阵汇总 ================")
	hdr := fmt.Sprintf("%-8s %-15s %11s %9s %9s %9s %10s %11s %10s",
		"workers", "consumers", "submit/s", "成功%", "p50(ms)", "p95(ms)", "p99(ms)", "task/s", "e2e p95(ms)")
	fmt.Println(hdr)
	fmt.Println(strings.Repeat("-", len(hdr)))
	for _, r := range rs {
		fmt.Printf("%-8d %-15s %11.1f %9.2f %9.2f %10.2f %9.2f %10.1f %11.2f\n",
			r.Workers,
			consumersLabel(r.Consumers),
			r.Submit.RPS,
			r.Submit.SuccessRate,
			r.Submit.Latency.P50,
			r.Submit.Latency.P95,
			r.Submit.Latency.P99,
			r.Drain.TaskPerSec,
			r.Drain.Latency.P95,
		)
	}
	fmt.Println(strings.Repeat("-", len(hdr)))
	fmt.Println("submit/s   = HTTP 提交吞吐（POST /api/tasks）")
	fmt.Println("task/s     = 任务真正执行落地的吞吐（排水阶段）")
	fmt.Println("e2e p95    = 任务 created_at → finished_at 的端到端时延 p95")
	fmt.Println("落地率     = drain.succeeded / (succeeded+failed)，见 summary.json")
	fmt.Println("==============================================")
}

func writeSummary(dir string, rs []*report) {
	b, _ := json.MarshalIndent(rs, "", "  ")
	path := filepath.Join(dir, "summary.json")
	if err := os.WriteFile(path, b, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "写 summary.json 失败: %v\n", err)
		return
	}
	fmt.Printf("\n汇总 JSON 已写入 %s\n", path)
}
