package database

import (
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// mustParse 解析 DSN 但不建连接——本文件全部用例都只验证配置映射，不依赖真实 PG。
func mustParse(t *testing.T, dsn string) *pgxpool.Config {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("解析 DSN: %v", err)
	}
	return cfg
}

// 连接池配错是典型的「线上才发现」。这些用例把可判定的问题钉在启动阶段：
// Warnings 返回非空就说明配置有问题，main 会把它打成 WARN 日志。
//
// 尤其是 MaxConns < WORKER_COUNT 这一条——它正是
// 「worker 都在跑但吞吐上不去」的成因，而且现象会随部署机器核数变化
// （pgxpool 默认 MaxConns = max(4, NumCPU)），事后极难归因。

func TestPoolOptionsWarnings(t *testing.T) {
	cases := []struct {
		name       string
		opt        PoolOptions
		workers    int
		wantSubstr string // 空串表示期望没有任何告警
	}{
		{
			name:       "健康配置不应有告警",
			opt:        PoolOptions{MaxConns: 25, MinConns: 5, ConnectTimeout: 5 * time.Second},
			workers:    20,
			wantSubstr: "",
		},
		{
			name:       "池比执行并发度还小",
			opt:        PoolOptions{MaxConns: 4, MinConns: 1, ConnectTimeout: 5 * time.Second},
			workers:    20,
			wantSubstr: "小于 WORKER_COUNT",
		},
		{
			name:       "MaxConns 未设置会退回随机器变化的默认值",
			opt:        PoolOptions{MaxConns: 0, ConnectTimeout: 5 * time.Second},
			workers:    20,
			wantSubstr: "max(4, NumCPU)",
		},
		{
			name:       "MinConns 大于 MaxConns 会被静默钳制",
			opt:        PoolOptions{MaxConns: 10, MinConns: 50, ConnectTimeout: 5 * time.Second},
			workers:    5,
			wantSubstr: "静默钳制",
		},
		{
			name:       "单实例占用超过 PG 默认上限的一半",
			opt:        PoolOptions{MaxConns: 80, MinConns: 5, ConnectTimeout: 5 * time.Second},
			workers:    20,
			wantSubstr: "max_connections",
		},
		{
			name:       "未设置连接超时",
			opt:        PoolOptions{MaxConns: 25, MinConns: 5},
			workers:    20,
			wantSubstr: "ConnectTimeout",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.opt.Warnings(tc.workers)
			if tc.wantSubstr == "" {
				if len(got) != 0 {
					t.Fatalf("期望无告警，实际 %v", got)
				}
				return
			}
			joined := strings.Join(got, " | ")
			if !strings.Contains(joined, tc.wantSubstr) {
				t.Fatalf("期望告警里包含 %q，实际 %v", tc.wantSubstr, got)
			}
		})
	}
}

// workers<=0 时不应因为「池比 worker 小」而误报——
// 有些调用方（seed 工具、测试）拿不到 worker 数。
func TestPoolOptionsWarningsSkipsWorkerCheckWhenUnknown(t *testing.T) {
	opt := PoolOptions{MaxConns: 2, MinConns: 1, ConnectTimeout: time.Second}
	for _, w := range []int{0, -1} {
		for _, msg := range opt.Warnings(w) {
			if strings.Contains(msg, "WORKER_COUNT") {
				t.Fatalf("workers=%d 时不应做 worker 数校验，实际告警: %s", w, msg)
			}
		}
	}
}

// applyTo 必须把配置真正写进去；MinConns 还要先被钳到 MaxConns，
// 否则日志里显示的值和实际生效的值会对不上。
func TestPoolOptionsApplyTo(t *testing.T) {
	opt := PoolOptions{
		MaxConns:          25,
		MinConns:          5,
		MaxConnLifetime:   2 * time.Hour,
		MaxConnIdleTime:   10 * time.Minute,
		ConnectTimeout:    3 * time.Second,
		HealthCheckPeriod: 45 * time.Second,
	}
	cfg := mustParse(t, "postgres://u:p@127.0.0.1:5432/db?sslmode=disable")
	opt.applyTo(cfg)

	if cfg.MaxConns != 25 {
		t.Errorf("MaxConns 应为 25，实际 %d", cfg.MaxConns)
	}
	if cfg.MinConns != 5 {
		t.Errorf("MinConns 应为 5，实际 %d", cfg.MinConns)
	}
	if cfg.MaxConnLifetime != 2*time.Hour {
		t.Errorf("MaxConnLifetime 应为 2h，实际 %v", cfg.MaxConnLifetime)
	}
	if cfg.MaxConnIdleTime != 10*time.Minute {
		t.Errorf("MaxConnIdleTime 应为 10m，实际 %v", cfg.MaxConnIdleTime)
	}
	if cfg.ConnConfig.ConnectTimeout != 3*time.Second {
		t.Errorf("ConnectTimeout 应为 3s，实际 %v", cfg.ConnConfig.ConnectTimeout)
	}
	if cfg.HealthCheckPeriod != 45*time.Second {
		t.Errorf("HealthCheckPeriod 应为 45s，实际 %v", cfg.HealthCheckPeriod)
	}
}

// MinConns > MaxConns 时必须被钳制，且不能报错。
func TestPoolOptionsApplyToClampsMinConns(t *testing.T) {
	cfg := mustParse(t, "postgres://u:p@127.0.0.1:5432/db?sslmode=disable")
	PoolOptions{MaxConns: 8, MinConns: 99}.applyTo(cfg)

	if cfg.MinConns != 8 {
		t.Fatalf("MinConns 应被钳制到 MaxConns(8)，实际 %d", cfg.MinConns)
	}
}

// 零值 PoolOptions 必须完全沿用 pgxpool 的默认值——这是给
// 「不想调池」的调用方（集成测试、一次性工具）留的后路，
// 语义必须明确：零值 = 不干预，而不是 = 把池设成 0。
func TestPoolOptionsZeroValueKeepsDefaults(t *testing.T) {
	cfg := mustParse(t, "postgres://u:p@127.0.0.1:5432/db?sslmode=disable")
	before := *cfg
	PoolOptions{}.applyTo(cfg)

	if cfg.MaxConns != before.MaxConns {
		t.Errorf("零值不应改动 MaxConns（%d → %d）", before.MaxConns, cfg.MaxConns)
	}
	if cfg.MinConns != 0 {
		t.Errorf("零值不应把 MinConns 抬起来，实际 %d", cfg.MinConns)
	}
	if cfg.MaxConnLifetime != before.MaxConnLifetime {
		t.Errorf("零值不应改动 MaxConnLifetime（%v → %v）", before.MaxConnLifetime, cfg.MaxConnLifetime)
	}
	// pgxpool 的默认 MaxConns 是 max(4, NumCPU)，至少为 4
	if cfg.MaxConns < 4 {
		t.Errorf("pgxpool 默认 MaxConns 至少应为 4，实际 %d", cfg.MaxConns)
	}
}
