// Package database 负责 PostgreSQL 连接与 schema 迁移。
// 迁移 SQL 通过 go:embed 打包进二进制，启动时按文件名顺序执行，
// 使用 advisory lock 保证多实例并发启动时只迁移一次。
package database

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

type DB struct {
	Pool *pgxpool.Pool
}

// PoolOptions 是连接池的显式配置。
//
// 为什么不用 pgxpool 的默认值：实测 v5.11.0 的默认值是
//
//	MaxConns = max(4, NumCPU)   ← 随部署机器核数变化，且与 WORKER_COUNT 无关
//	MinConns = 0                ← 池会缩到 0，空闲后第一波请求要现付握手
//	ConnectTimeout = 0          ← 不设 dialer 超时，靠 OS 的 TCP 重传兜底
//
// 前两条决定了「池大小」这个最常被追问的参数在这份代码里**没有答案**——
// 32 核机器上是 32，4 核机器上是 4，而服务的 WORKER_COUNT 配置是 20。
// 池比 worker 还小的时候，worker 会全部堵在池上排队，
// 表现为"worker 都在跑但吞吐上不去"，而且这个现象在核数不同的机器上还不一样。
type PoolOptions struct {
	// MaxConns 是池内连接数上限。
	// 定值规则：≥ 节点执行并发度 + HTTP 并发度，
	// 且必须满足「单实例 MaxConns × 实例数 < PG 的 max_connections」。
	MaxConns int32
	// MinConns 是池保持的最小连接数（热连接，避免冷启动握手）。
	MinConns int32
	// MaxConnLifetime 是单条连接的最大存活时长。
	MaxConnLifetime time.Duration
	// MaxConnIdleTime 是空闲连接被回收前的等待时长。
	MaxConnIdleTime time.Duration
	// ConnectTimeout 是建立单条连接的超时。
	ConnectTimeout time.Duration
	// HealthCheckPeriod 是后台健康检查的周期（默认 1 分钟）。
	HealthCheckPeriod time.Duration
}

// Warnings 返回配置里值得提醒但不致命的问题。
//
// 做成返回值而不是直接打日志，是为了能写测试——
// 连接池配错是典型的"线上才发现"，能在启动阶段用断言钉住的东西不该靠人工 review。
// workers 传节点执行并发度（WORKER_COUNT），用于判断池是否比执行并发度还小。
func (o PoolOptions) Warnings(workers int) []string {
	var w []string
	if o.MaxConns <= 0 {
		w = append(w, "MaxConns <= 0：pgxpool 会退回默认值 max(4, NumCPU)，池大小将随部署机器变化")
	}
	if o.MaxConns > 0 && o.MinConns > o.MaxConns {
		w = append(w, fmt.Sprintf(
			"MinConns(%d) 大于 MaxConns(%d)：pgxpool 会把 MinConns 静默钳制到 MaxConns", o.MinConns, o.MaxConns))
	}
	if workers > 0 && int(o.MaxConns) < workers {
		w = append(w, fmt.Sprintf(
			"MaxConns(%d) 小于 WORKER_COUNT(%d)：节点执行会全部堵在池上排队，"+
				"表现为 worker 都在跑但吞吐上不去", o.MaxConns, workers))
	}
	// PG 默认 max_connections=100。单实例就吃掉一大半时，多实例部署必然打满。
	if o.MaxConns > 50 {
		w = append(w, fmt.Sprintf(
			"MaxConns(%d) 偏大：PostgreSQL 默认 max_connections=100，"+
				"单实例占用超过一半时多实例部署会打满服务端连接", o.MaxConns))
	}
	if o.ConnectTimeout <= 0 {
		w = append(w, "ConnectTimeout 未设置：PG 不可达时实际超时取决于操作系统 TCP 重传（约 2 分钟），建议显式设置")
	}
	return w
}

// applyTo 把配置写到 pgxpool.Config 上。
func (o PoolOptions) applyTo(cfg *pgxpool.Config) {
	if o.MaxConns > 0 {
		cfg.MaxConns = o.MaxConns
	}
	if o.MinConns > 0 {
		// pgxpool 会把 MinConns 钳到 MaxConns，这里先钳一次，
		// 让 cfg 反映真正生效的值（否则日志和实际行为会对不上）。
		cfg.MinConns = min(o.MinConns, cfg.MaxConns)
	}
	if o.MaxConnLifetime > 0 {
		cfg.MaxConnLifetime = o.MaxConnLifetime
	}
	if o.MaxConnIdleTime > 0 {
		cfg.MaxConnIdleTime = o.MaxConnIdleTime
	}
	if o.ConnectTimeout > 0 {
		cfg.ConnConfig.ConnectTimeout = o.ConnectTimeout
	}
	if o.HealthCheckPeriod > 0 {
		cfg.HealthCheckPeriod = o.HealthCheckPeriod
	}
}

// Connect 建立连接池。opt 为零值时退回 pgxpool 的默认行为（仅为兼容旧调用方）。
func Connect(ctx context.Context, url string, opt PoolOptions) (*DB, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, fmt.Errorf("parse postgres url: %w", err)
	}
	opt.applyTo(cfg)

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("connect postgres: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping postgres: %w", err)
	}
	return &DB{Pool: pool}, nil
}

// PoolStats 是连接池的运行时快照，用于上报指标。
//
// 其中 EmptyAcquire 与 CanceledAcquire 是判断「池够不够大」的决定性指标：
//   - EmptyAcquire 计数每次调用方因为池里没有空闲连接而必须等待（含新建连接）；
//   - CanceledAcquire 计数等待被 context 取消的次数——这就是"吞吐上不去"的直接证据。
//
// 只调大 MaxConns 而不看这两个数，是在猜。
type PoolStats struct {
	TotalConns        int32
	IdleConns         int32
	AcquiredConns     int32
	ConstructingConns int32
	MaxConns          int32
	EmptyAcquire      int64
	CanceledAcquire   int64
	NewConns          int64
}

// Stats 返回连接池当前快照；池未初始化时返回零值。
func (d *DB) Stats() PoolStats {
	if d == nil || d.Pool == nil {
		return PoolStats{}
	}
	s := d.Pool.Stat()
	return PoolStats{
		TotalConns:        s.TotalConns(),
		IdleConns:         s.IdleConns(),
		AcquiredConns:     s.AcquiredConns(),
		ConstructingConns: s.ConstructingConns(),
		MaxConns:          s.MaxConns(),
		EmptyAcquire:      s.EmptyAcquireCount(),
		CanceledAcquire:   s.CanceledAcquireCount(),
		NewConns:          s.NewConnsCount(),
	}
}

// Migrate 顺序执行 embedded migrations。每个文件在 schema_migrations 中记录。
//
// 多实例并发保护：pg_advisory_lock 是"会话级"锁，必须从连接池里取一条
// 固定连接来执行加锁/解锁/迁移全过程。原实现直接用 pool.Exec 加锁，
// 解锁时可能落到另一条连接上——锁解不掉，第二个实例会永久卡在迁移阶段。
func (d *DB) Migrate(ctx context.Context) error {
	entries, err := fs.ReadDir(migrationsFS, "migrations")
	if err != nil {
		return err
	}
	var files []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".sql") {
			files = append(files, e.Name())
		}
	}
	sort.Strings(files)
	if len(files) == 0 {
		return nil
	}

	// 整段迁移复用同一条连接，保证 advisory lock 的加解锁落在同一会话
	conn, err := d.Pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire migration connection: %w", err)
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version TEXT PRIMARY KEY,
		applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
	)`); err != nil {
		return err
	}

	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, migrateLockKey); err != nil {
		return fmt.Errorf("acquire migration lock: %w", err)
	}
	defer func() {
		// 用后台上下文解锁：迁移失败时 ctx 可能已经取消
		unlockCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := conn.Exec(unlockCtx, `SELECT pg_advisory_unlock($1)`, migrateLockKey); err != nil {
			fmt.Printf("warning: release migration lock failed: %v\n", err)
		}
	}()

	for _, f := range files {
		var applied bool
		err := conn.QueryRow(ctx,
			`SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE version=$1)`, f).Scan(&applied)
		if err != nil {
			return err
		}
		if applied {
			continue
		}
		sqlBytes, err := fs.ReadFile(migrationsFS, "migrations/"+f)
		if err != nil {
			return err
		}
		if _, err := conn.Exec(ctx, string(sqlBytes)); err != nil {
			return fmt.Errorf("apply migration %s: %w", f, err)
		}
		if _, err := conn.Exec(ctx,
			`INSERT INTO schema_migrations(version) VALUES($1)`, f); err != nil {
			return err
		}
	}
	return nil
}

// migrateLockKey 是本服务专用的 advisory lock 常量。
const migrateLockKey = 727001

// EnsureVectorSchema 把 document_chunks 的向量存储收敛到「可被索引检索」的状态。
//
// 做三件事，顺序有讲究：
//  1. 校验 embedding_v 的维度等于 dim，不一致就 ALTER 修正；
//  2. 建 HNSW 余弦索引；
//  3. 删掉历史的 JSONB embedding 列。
//
// 为什么这三件事不写在 SQL 迁移文件里：
//   - 维度来自配置（EMBED_DIM），SQL 文件无法参数化；
//   - pgvector 的 HNSW 索引要求列有确定维度，而 002 迁移建的是无维度列
//     （实测：对无维度列建索引直接报 "column does not have dimensions"）。
//
// 为什么删列放在最后：只有确认向量列已能正确索引，才丢弃源数据。
// 中途任何一步失败都直接返回，历史 embedding 原封不动，可以安全重跑。
//
// 注意 CREATE EXTENSION 在 002 迁移里执行，需要超级用户权限——
// pgvector 的 control 文件没有 trusted=true，普通角色建不了扩展。
func (d *DB) EnsureVectorSchema(ctx context.Context, dim int) error {
	if dim <= 0 {
		return fmt.Errorf("EMBED_DIM 必须是正整数，当前为 %d", dim)
	}

	var current string
	err := d.Pool.QueryRow(ctx, `
		SELECT format_type(atttypid, atttypmod)
		  FROM pg_attribute
		 WHERE attrelid = 'document_chunks'::regclass
		   AND attname  = 'embedding_v'
		   AND NOT attisdropped`).Scan(&current)
	if errors.Is(err, pgx.ErrNoRows) {
		return errors.New("document_chunks.embedding_v 不存在：迁移 002_pgvector.sql 未生效")
	}
	if err != nil {
		return fmt.Errorf("读取 embedding_v 列类型: %w", err)
	}

	want := fmt.Sprintf("vector(%d)", dim)
	if current != want {
		// 已有数据必须全部匹配 dim，否则 ALTER 会失败。这是刻意的：
		// 维度对不上说明 Embedder 换过，必须显式重新索引整个知识库，
		// 而不是让一批错误维度的向量静默留在库里、检索时给出无意义结果。
		if _, err := d.Pool.Exec(ctx,
			fmt.Sprintf(`ALTER TABLE document_chunks ALTER COLUMN embedding_v TYPE %s`, want),
		); err != nil {
			return fmt.Errorf(
				"把 embedding_v 从 %s 改为 %s 失败（EMBED_DIM 与库中已有向量维度不一致，需清空 document_chunks 后重新索引）: %w",
				current, want, err)
		}
	}

	// HNSW + 余弦距离。vector_cosine_ops 与查询侧的 `<=>` 必须配套，
	// 否则索引不会被规划器命中（会退化成顺序扫描）。
	if _, err := d.Pool.Exec(ctx, `
		CREATE INDEX IF NOT EXISTS idx_chunks_embedding_hnsw
		ON document_chunks USING hnsw (embedding_v vector_cosine_ops)`); err != nil {
		return fmt.Errorf("创建 HNSW 索引: %w", err)
	}

	// 向量列已就绪，丢弃历史的 JSONB 列（数据已在 002 迁移里回填）
	if _, err := d.Pool.Exec(ctx,
		`ALTER TABLE document_chunks DROP COLUMN IF EXISTS embedding`); err != nil {
		return fmt.Errorf("删除历史 embedding 列: %w", err)
	}
	return nil
}

func (d *DB) Close() {
	if d.Pool != nil {
		d.Pool.Close()
	}
}
