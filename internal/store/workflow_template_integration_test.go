package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/hoarfrost/nebulaflow/internal/model"
	"github.com/jackc/pgx/v5/pgconn"
)

// 这组用例覆盖"系统模板写不进库 → 线上模板市场为空"这条缺陷。
//
// 缺陷：模板市场把系统模板的归属写成 user_id = 0，但
//   001_init.sql: user_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE
// 而 users.id 是 BIGSERIAL（永远从 1 开始）—— 库里不存在 id=0 的用户，
// 于是每一条系统模板 INSERT 都被外键拒绝（SQLSTATE 23503 workflows_user_id_fkey）。
// 内存实现不做外键校验，所以 internal/store 的单测一直是绿的，只有 PG 路径烂，
// 而 cmd/seed 当时又只 log.Printf（退出码 0）—— 故障连日志都看不出是致命问题。
//
// 修复：010_system_template_owner.sql 放开 NOT NULL，用 NULL 表示"无归属"的系统模板；
// store 在读写边界翻译 model.SystemTemplateUserID(0) <-> NULL。
// 下面断言的是这条真实 SQL 路径的语义，包括"归属校验不放松"。
//
// 用例名带 MultiTenant 是为了落进 ci.yml 那条真库集成步骤的 -run 过滤
// （'MultiTenant|Knowledge|Document|TaskPriority|Hybrid'）：模板可见性本来就是
// 多租户隔离问题 —— 系统模板对所有租户可见，私有模板只对创建者可见。

// tplTestWorkflow 造一个测试用模板（系统模板用 UserID=0）。
func tplTestWorkflow(userID int64, name, category string) *model.Workflow {
	return &model.Workflow{
		UserID: userID, IsTemplate: true, Category: category, Icon: "🧪",
		Name: name, Description: "store 集成用例",
		Status: model.WorkflowPublished,
		Nodes: []model.WorkflowNode{
			{NodeKey: "input", NodeType: model.NodeInput, PositionX: 1, PositionY: 2},
			{NodeKey: "llm", NodeType: model.NodeLLM, PositionX: 3, PositionY: 4,
				Config: model.NodeConfig{Model: "mock-chat", Prompt: "ping"}},
			{NodeKey: "output", NodeType: model.NodeOutput},
		},
		Edges: []model.WorkflowEdge{
			{SourceNode: "input", TargetNode: "llm"},
			{SourceNode: "llm", TargetNode: "output"},
		},
	}
}

func TestMultiTenant_SystemTemplateOwnerlessUserID(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	db := pgTestDB(t, ctx, 384)
	repo := &WorkflowRepo{db: db}
	ownerID := pgTestUserID(t, ctx, db, "tpl-owner")
	otherID := pgTestUserID(t, ctx, db, "tpl-other")

	// 用固定前缀命名，便于按名字清理（system 模板的 owner 是 NULL，
	// 不能靠 user_id 定位测试数据）。
	const sysName = "store-int-system-tpl"
	const sysCat = "store-int-syscat"
	cleanupNames := []string{sysName}
	t.Cleanup(func() {
		if _, err := db.Pool.Exec(context.Background(),
			`DELETE FROM workflows WHERE name = ANY($1)`, cleanupNames); err != nil {
			t.Logf("清理测试模板失败: %v", err)
		}
	})
	// 从"库里没有"开始，避免上一次中断留下的半成品影响断言
	if _, err := db.Pool.Exec(ctx, `DELETE FROM workflows WHERE name = ANY($1)`, cleanupNames); err != nil {
		t.Fatalf("准备测试数据（清理同名模板）: %v", err)
	}

	// ---- 1. 缺陷点本身：写入"系统模板"（Go 侧 UserID=0）必须成功 ----
	sys := tplTestWorkflow(model.SystemTemplateUserID, sysName, sysCat)
	if err := repo.CreateWorkflow(ctx, sys); err != nil {
		var pgErr *pgconn.PgError
		hint := ""
		if errors.As(err, &pgErr) && pgErr.Code == "23503" {
			hint = "（这就是线上模板市场为空的原因：workflows_user_id_fkey 拒绝了 user_id=0）"
		}
		t.Fatalf("CreateWorkflow(系统模板 user_id=0) 失败: %v%s", err, hint)
	}
	if sys.ID == 0 {
		t.Fatal("CreateWorkflow 未回填 ID")
	}

	// ---- 2. 落库形态：user_id 必须是 NULL，而不是 0/其他值 ----
	var rawOwner *int64
	if err := db.Pool.QueryRow(ctx,
		`SELECT user_id FROM workflows WHERE id=$1`, sys.ID).Scan(&rawOwner); err != nil {
		t.Fatalf("直接查库读 workflows.user_id: %v", err)
	}
	if rawOwner != nil {
		t.Fatalf("系统模板落库的 user_id 应为 NULL（无归属用户），实际为 %d", *rawOwner)
	}

	// ---- 3. 模板市场：任意用户都应看到系统模板，且读回 Go 侧仍是哨兵值 0 ----
	visible, err := repo.ListTemplates(ctx, otherID, "")
	if err != nil {
		t.Fatalf("ListTemplates: %v", err)
	}
	var got *model.Workflow
	for i := range visible {
		if visible[i].ID == sys.ID {
			got = &visible[i]
		}
	}
	if got == nil {
		t.Fatalf("ListTemplates(other user) 没有返回系统模板 %d（模板市场会是空的）", sys.ID)
	}
	if got.UserID != model.SystemTemplateUserID {
		t.Fatalf("系统模板读回 user_id=%d，期望哨兵值 %d", got.UserID, model.SystemTemplateUserID)
	}
	if !got.IsTemplate || got.Name != sysName || got.Category != sysCat {
		t.Fatalf("系统模板字段不对: %+v", *got)
	}

	// 分类过滤 + 分类列表（前端分类筛选器依赖这两个接口）
	byCat, err := repo.ListTemplates(ctx, otherID, sysCat)
	if err != nil {
		t.Fatalf("ListTemplates(category=%s): %v", sysCat, err)
	}
	if len(byCat) != 1 || byCat[0].ID != sys.ID {
		t.Fatalf("按分类 %q 过滤应只命中系统模板 %d，实际 %d 条", sysCat, sys.ID, len(byCat))
	}
	cats, err := repo.ListTemplateCategories(ctx, otherID)
	if err != nil {
		t.Fatalf("ListTemplateCategories: %v", err)
	}
	if !containsStr(cats, sysCat) {
		t.Fatalf("分类列表 %v 缺少系统模板的分类 %q", cats, sysCat)
	}

	// ---- 4. 模板详情：节点/边要能预览出来 ----
	detail, err := repo.GetTemplate(ctx, sys.ID, otherID)
	if err != nil {
		t.Fatalf("GetTemplate: %v", err)
	}
	if len(detail.Nodes) != 3 || len(detail.Edges) != 2 {
		t.Fatalf("模板详情 nodes/edges = %d/%d，期望 3/2", len(detail.Nodes), len(detail.Edges))
	}
	if detail.UserID != model.SystemTemplateUserID {
		t.Fatalf("模板详情 user_id=%d，期望 %d", detail.UserID, model.SystemTemplateUserID)
	}

	// ---- 5. 归属校验不能被放松：系统模板不等于"大家的普通工作流" ----
	if _, err := repo.GetWorkflow(ctx, sys.ID, otherID); err == nil {
		t.Fatal("GetWorkflow 不应把系统模板当成普通用户的 workflow 返回（越权）")
	}
	// 任何用户都不能改系统模板：UpdateWorkflow 用别人的 user_id 时必须 NotFound
	hijack := tplTestWorkflow(otherID, sysName, sysCat)
	hijack.ID = sys.ID
	if err := repo.UpdateWorkflow(ctx, hijack); err == nil {
		t.Fatal("UpdateWorkflow 不应允许用户改写系统模板（越权）")
	}
	// 也不能删
	if err := repo.DeleteWorkflow(ctx, sys.ID, otherID); err == nil {
		t.Fatal("DeleteWorkflow 不应允许用户删除系统模板（越权）")
	}
	// 系统模板不能混进用户自己的工作流列表
	mine, err := repo.ListWorkflows(ctx, otherID)
	if err != nil {
		t.Fatalf("ListWorkflows: %v", err)
	}
	for _, w := range mine {
		if w.ID == sys.ID {
			t.Fatal("ListWorkflows 不应包含系统模板")
		}
	}

	// ---- 6. 系统模板自己能改（IS NOT DISTINCT FROM 的 NULL 侧要能匹配上）----
	update := tplTestWorkflow(model.SystemTemplateUserID, sysName, sysCat)
	update.ID = sys.ID
	update.Description = "seed 更新后的描述"
	if err := repo.UpdateWorkflow(ctx, update); err != nil {
		t.Fatalf("UpdateWorkflow(系统模板, user_id=0 → NULL): %v", err)
	}
	after, err := repo.GetTemplate(ctx, sys.ID, otherID)
	if err != nil {
		t.Fatalf("GetTemplate(更新后): %v", err)
	}
	if after.Description != "seed 更新后的描述" {
		t.Fatalf("系统模板更新未生效: description=%q", after.Description)
	}

	// ---- 7. 一键使用：复制出来的工作流属于调用者，不再是模板 ----
	copied, err := repo.CreateFromTemplate(ctx, sys.ID, otherID, "")
	if err != nil {
		t.Fatalf("CreateFromTemplate: %v", err)
	}
	cleanupNames = append(cleanupNames, copied.Name)
	if copied.UserID != otherID || copied.IsTemplate {
		t.Fatalf("从模板创建的工作流应归属 %d 且 is_template=false，实际 user_id=%d is_template=%v",
			otherID, copied.UserID, copied.IsTemplate)
	}
	if len(copied.Nodes) != 3 {
		t.Fatalf("复制出的工作流 nodes=%d，期望 3", len(copied.Nodes))
	}
	if _, err := repo.GetWorkflow(ctx, copied.ID, otherID); err != nil {
		t.Fatalf("复制出的工作流应能被所有者读到: %v", err)
	}

	// ---- 8. 用户自己的私有模板：只对自己可见，不受 NULL 化影响 ----
	priv := tplTestWorkflow(ownerID, "store-int-user-tpl", "store-int-usercat")
	if err := repo.CreateWorkflow(ctx, priv); err != nil {
		t.Fatalf("CreateWorkflow(用户私有模板): %v", err)
	}
	cleanupNames = append(cleanupNames, priv.Name)
	ownerView, err := repo.ListTemplates(ctx, ownerID, "")
	if err != nil {
		t.Fatalf("ListTemplates(owner): %v", err)
	}
	if countID(ownerView, priv.ID) != 1 {
		t.Fatal("私有模板的创建者应该能在模板列表里看到它")
	}
	if countID(ownerView, sys.ID) != 1 {
		t.Fatal("私有模板的创建者也应该能看到系统模板")
	}
	otherView, err := repo.ListTemplates(ctx, otherID, "")
	if err != nil {
		t.Fatalf("ListTemplates(other): %v", err)
	}
	if countID(otherView, priv.ID) != 0 {
		t.Fatal("别的用户不应看到他人的私有模板")
	}
	if _, err := repo.GetTemplate(ctx, priv.ID, otherID); err == nil {
		t.Fatal("GetTemplate 不应允许他人读取私有模板（越权）")
	}

	// ---- 9. 非模板工作流不允许用 0：否则会静默造出一个无归属的普通工作流 ----
	bad := &model.Workflow{UserID: model.SystemTemplateUserID, Name: "store-int-bad-owner",
		Status: model.WorkflowDraft}
	err = repo.CreateWorkflow(ctx, bad)
	if err == nil {
		cleanupNames = append(cleanupNames, bad.Name)
		t.Fatal("非模板工作流用 user_id=0 应被拒绝")
	}
	if !strings.Contains(err.Error(), "系统模板") {
		t.Fatalf("拒绝原因应说明 0 是系统模板专用哨兵值，实际: %v", err)
	}
}

func countID(list []model.Workflow, id int64) int {
	n := 0
	for _, w := range list {
		if w.ID == id {
			n++
		}
	}
	return n
}

func containsStr(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}
