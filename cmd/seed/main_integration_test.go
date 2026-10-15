package main

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/hoarfrost/nebulaflow/internal/database"
	"github.com/hoarfrost/nebulaflow/internal/model"
	"github.com/hoarfrost/nebulaflow/internal/store"
)

// systemTemplateNames 与 seedTemplates 里的 5 个系统模板一一对应。
// 这里刻意写死名字：用例要断言的正是"线上模板市场应该有这几条"。
var systemTemplateNames = []string{"智能客服", "代码助手", "知识问答", "数据分析报告", "文案写作"}

// TestSeedTemplates_WritesSystemTemplatesIntoRealDB 跑的是 cmd/seed 的真实写入
// 路径（同一个 seedTemplates，线上 nebulaflow-seed 用的就是它）。
//
// 缺陷：seed 用 user_id = 0 写系统模板，而 workflows.user_id 是
// BIGINT NOT NULL REFERENCES users(id)（users.id 从 1 开始）—— 0 不是合法用户，
// 每一条模板 INSERT 都被外键拒绝（SQLSTATE 23503 workflows_user_id_fkey）。
// 旧实现只 log.Printf 掉错误、退出码仍是 0，于是线上模板市场一直是空的，
// 而 CI 全绿。
//
// 修复后本用例断言三件事：
//  1. seedTemplates 返回 nil（不再有 FK 错误，失败会走非零退出码）；
//  2. 库里真的存在这 5 行，且 user_id IS NULL（无归属的系统模板）；
//  3. 任意用户都能通过 ListTemplates 看到它们（模板市场不再是空数组），
//     并且重复执行是幂等的（不新建第二份）。
func TestSeedTemplates_WritesSystemTemplatesIntoRealDB(t *testing.T) {
	dsn := os.Getenv("NEBULA_TEST_DSN")
	if dsn == "" {
		t.Skip("未设置 NEBULA_TEST_DSN，跳过需要真实 PostgreSQL 的集成测试")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	db, err := database.Connect(ctx, dsn, database.PoolOptions{MaxConns: 4, MinConns: 1})
	if err != nil {
		t.Fatalf("连接 PostgreSQL: %v", err)
	}
	t.Cleanup(db.Pool.Close)
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("执行迁移: %v", err)
	}
	st := store.New(db)

	// 清理只按名字删（系统模板的 user_id 是 NULL，不能按归属定位）。
	// 用例开始时也先清一遍，保证从"库里没有系统模板"这个前提出发。
	purge := func() {
		if _, err := db.Pool.Exec(context.Background(),
			`DELETE FROM workflows WHERE name = ANY($1)`, systemTemplateNames); err != nil {
			t.Logf("清理系统模板失败: %v", err)
		}
	}
	purge()
	t.Cleanup(purge)

	// ---- 1. 修复前这里必然失败：SQLSTATE 23503 workflows_user_id_fkey ----
	if err := seedTemplates(ctx, st); err != nil {
		t.Fatalf("seedTemplates 失败（修复前即 workflows_user_id_fkey: SQLSTATE 23503）: %v", err)
	}

	// ---- 2. 落库形态：5 行系统模板，且全部无归属（user_id IS NULL） ----
	var total, ownerless int
	if err := db.Pool.QueryRow(ctx,
		`SELECT count(*), count(*) FILTER (WHERE user_id IS NULL)
		   FROM workflows WHERE is_template AND name = ANY($1)`,
		systemTemplateNames).Scan(&total, &ownerless); err != nil {
		t.Fatalf("统计系统模板: %v", err)
	}
	if total != len(systemTemplateNames) {
		t.Fatalf("库里系统模板 %d 条，期望 %d 条", total, len(systemTemplateNames))
	}
	if ownerless != total {
		t.Fatalf("系统模板中 user_id IS NULL 的有 %d/%d 条，期望全部为 NULL", ownerless, total)
	}

	// ---- 3. 模板市场视角：一个"陌生"用户也要能看到全部 5 条 ----
	// userID 故意不落库：模板可见性不该依赖"这个用户存在/是模板的谁"。
	const strangerID int64 = 987654321
	list, err := st.Workflows.ListTemplates(ctx, strangerID, "")
	if err != nil {
		t.Fatalf("ListTemplates: %v", err)
	}
	seen := make(map[string]model.Workflow, len(list))
	for _, w := range list {
		seen[w.Name] = w
	}
	for _, name := range systemTemplateNames {
		w, ok := seen[name]
		if !ok {
			t.Fatalf("模板市场查不到系统模板 %q（/api/templates 会是空的），当前可见: %d 条 %v",
				name, len(list), namesOf(list))
		}
		if w.UserID != model.SystemTemplateUserID {
			t.Fatalf("系统模板 %q 读回 user_id=%d，期望哨兵值 %d", name, w.UserID, model.SystemTemplateUserID)
		}
		if w.Category == "" {
			t.Fatalf("系统模板 %q 缺少分类（分类筛选器会漏掉它）", name)
		}
	}
	cats, err := st.Workflows.ListTemplateCategories(ctx, strangerID)
	if err != nil {
		t.Fatalf("ListTemplateCategories: %v", err)
	}
	if len(cats) == 0 {
		t.Fatal("模板分类列表为空")
	}

	// ---- 4. 幂等：重复部署时 seed 会再跑一次，不能产生重复模板或报错 ----
	if err := seedTemplates(ctx, st); err != nil {
		t.Fatalf("第二次 seedTemplates（幂等重跑）失败: %v", err)
	}
	var after int
	if err := db.Pool.QueryRow(ctx,
		`SELECT count(*) FROM workflows WHERE is_template AND name = ANY($1)`,
		systemTemplateNames).Scan(&after); err != nil {
		t.Fatalf("重跑后统计: %v", err)
	}
	if after != total {
		t.Fatalf("幂等重跑后系统模板变成 %d 条（原 %d 条）", after, total)
	}
}

func namesOf(list []model.Workflow) []string {
	out := make([]string, 0, len(list))
	for _, w := range list {
		out = append(out, w.Name)
	}
	return out
}
