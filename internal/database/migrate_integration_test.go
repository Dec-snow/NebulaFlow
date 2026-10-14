package database

import (
	"context"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// 本文件钉住一条部署级缺陷：**全新数据库上迁移跑不完**。
//
// 缺陷：004_agent_memory.sql 直接在「无维度」的 vector 列上建 HNSW 索引，
// pgvector 要求 HNSW 必须作用在定维列上，于是空库执行迁移时必然失败：
//
//	apply migration 004_agent_memory.sql:
//	ERROR: column does not have dimensions (SQLSTATE 22023)
//
// 为什么既有的测试拦不住：所有既有集成用例都跑在**已经迁移过的库**上
// （004 早就是 applied 状态，直接 skip），而 SQL 迁移的失败面恰恰只在
// 「这个库从来没见过这些迁移」的时候才暴露。CI 当时是靠一段 psql
// bootstrap 先把 agent_memory 建成 vector(384) 绕过去的——绕过去就等于
// 永远测不到，真实用户拿到的却是起不来的服务。
//
// 因此这里必须**创建一个全新的数据库**再跑 Migrate，而不是复用测试库。
func TestMigrateOnBrandNewDatabase(t *testing.T) {
	dsn := os.Getenv("NEBULA_TEST_DSN")
	if dsn == "" {
		t.Skip("未设置 NEBULA_TEST_DSN，跳过需要真实 PostgreSQL 的集成测试")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	admin, err := Connect(ctx, dsn, PoolOptions{})
	if err != nil {
		t.Fatalf("连接 PostgreSQL: %v", err)
	}
	// 用 t.Cleanup 而不是 defer：清理阶段（删库）必须先于关闭 admin 池执行。
	t.Cleanup(admin.Close)

	name := freshDBName()
	ident := pgx.Identifier{name}.Sanitize()

	// 先删一次，保证拿到的是真正的空库（上一次跑挂了可能留下同名库）。
	if _, err := admin.Pool.Exec(ctx, `DROP DATABASE IF EXISTS `+ident+` WITH (FORCE)`); err != nil {
		t.Fatalf("清理同名测试库 %s: %v", name, err)
	}
	if _, err := admin.Pool.Exec(ctx, `CREATE DATABASE `+ident); err != nil {
		t.Fatalf("创建测试库 %s: %v", name, err)
	}

	var fresh *DB
	t.Cleanup(func() {
		if fresh != nil {
			fresh.Close()
		}
		// 删库用后台上下文：用例失败时 ctx 可能已被取消。
		dropCtx, dropCancel := context.WithTimeout(context.Background(), time.Minute)
		defer dropCancel()
		// FORCE 兜底杀掉残留连接（pg16 支持），否则删库会因「有其他会话」失败。
		if _, err := admin.Pool.Exec(dropCtx,
			`DROP DATABASE IF EXISTS `+ident+` WITH (FORCE)`); err != nil {
			t.Logf("删除测试库 %s 失败: %v", name, err)
		}
	})

	// 连到空库上。改 Database 而不是拼 DSN，避免转义/参数顺序出错。
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("解析 DSN: %v", err)
	}
	cfg.ConnConfig.Database = name
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("连接测试库 %s: %v", name, err)
	}
	fresh = &DB{Pool: pool}
	if err := pool.Ping(ctx); err != nil {
		t.Fatalf("ping 测试库 %s: %v", name, err)
	}

	// ---- 被验证的核心断言：空库上迁移必须一次跑通 ----
	if err := fresh.Migrate(ctx); err != nil {
		t.Fatalf("空库执行迁移失败（这正是本节要拦住的部署级缺陷）: %v", err)
	}

	// 空库上迁移必须真的执行到 004，而不是被 schema_migrations 意外跳过。
	// （若 004 失败，它不会被记录，且 Migrate 会直接返回错误；
	//   这里再确认一次，防止「文件被删掉所以不再报错」这种假修复。）
	versions := appliedVersions(t, ctx, fresh)
	for _, want := range []string{"002_pgvector.sql", "004_agent_memory.sql"} {
		if !versions[want] {
			t.Fatalf("迁移 %s 未记录到 schema_migrations，实际已应用: %v", want, keys(versions))
		}
	}

	// ---- 空库上维度收敛 + HNSW 索引必须成功 ----
	if err := fresh.EnsureVectorSchema(ctx, 384); err != nil {
		t.Fatalf("空库收敛向量 schema 失败: %v", err)
	}

	// 每个向量列都应是 vector(384)，且带一个 hnsw 索引。
	// 直接遍历生产代码里的 vecColumns，新增向量列时这里自动覆盖。
	for _, spec := range vecColumns {
		if got := columnType(t, ctx, fresh, spec.Table, spec.Column); got != "vector(384)" {
			t.Errorf("%s.%s 应为 vector(384)，实际 %s", spec.Table, spec.Column, got)
		}
		def := indexDef(t, ctx, fresh, spec.Table, spec.Index)
		if def == "" {
			t.Errorf("%s 上缺少 HNSW 索引 %s", spec.Table, spec.Index)
			continue
		}
		if !strings.Contains(def, "hnsw") {
			t.Errorf("%s 不是 HNSW 索引: %s", spec.Index, def)
		}
		if !strings.Contains(def, "vector_cosine_ops") {
			t.Errorf("%s 应使用 vector_cosine_ops（与查询侧 <=> 配套）: %s", spec.Index, def)
		}
	}

	// 幂等：再跑一次不应报错，也不应重复建索引。
	if err := fresh.Migrate(ctx); err != nil {
		t.Errorf("重复执行迁移应幂等，实际报错: %v", err)
	}
	if err := fresh.EnsureVectorSchema(ctx, 384); err != nil {
		t.Errorf("重复收敛向量 schema 应幂等，实际报错: %v", err)
	}
}

// freshDBName 生成测试库名。带 pid 是为了让并行/残留的另一次运行不互相踩。
func freshDBName() string {
	return "nebulaflow_migtest_" + strconv.Itoa(os.Getpid())
}

func appliedVersions(t *testing.T, ctx context.Context, db *DB) map[string]bool {
	t.Helper()
	rows, err := db.Pool.Query(ctx, `SELECT version FROM schema_migrations`)
	if err != nil {
		t.Fatalf("读取 schema_migrations: %v", err)
	}
	defer rows.Close()
	got := map[string]bool{}
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			t.Fatalf("扫描 schema_migrations: %v", err)
		}
		got[v] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("遍历 schema_migrations: %v", err)
	}
	return got
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func columnType(t *testing.T, ctx context.Context, db *DB, table, column string) string {
	t.Helper()
	var typ string
	if err := db.Pool.QueryRow(ctx, `
		SELECT format_type(atttypid, atttypmod)
		  FROM pg_attribute
		 WHERE attrelid = $1::regclass AND attname = $2 AND NOT attisdropped`,
		table, column).Scan(&typ); err != nil {
		t.Fatalf("读取 %s.%s 列类型: %v", table, column, err)
	}
	return typ
}

func indexDef(t *testing.T, ctx context.Context, db *DB, table, index string) string {
	t.Helper()
	var def string
	err := db.Pool.QueryRow(ctx,
		`SELECT indexdef FROM pg_indexes WHERE tablename = $1 AND indexname = $2`,
		table, index).Scan(&def)
	if err != nil {
		if err == pgx.ErrNoRows {
			return ""
		}
		t.Fatalf("读取索引 %s 定义: %v", index, err)
	}
	return def
}
