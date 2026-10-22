package store

import (
	"context"
	"testing"

	"github.com/hoarfrost/nebulaflow/internal/model"
)

// TestListTasksLimitClampedNotReset 验证 P3-18：limit 超出上限时应**钳到 100**，
// 而不是被静默重置为默认 20 ——「要太多」与「没说要多少」是两种不同的请求，
// 压测工具曾因静默重置在分页/排水场景漏统计。
func TestListTasksLimitClampedNotReset(t *testing.T) {
	m := NewMemory()
	ctx := context.Background()

	u := &model.User{Username: "limituser", Email: "x@example.com", PasswordHash: "x"}
	if err := m.Users.CreateUser(ctx, u); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	wf := &model.Workflow{UserID: u.ID, Name: "wf"}
	if err := m.Workflows.CreateWorkflow(ctx, wf); err != nil {
		t.Fatalf("CreateWorkflow: %v", err)
	}
	for i := 0; i < 110; i++ {
		if err := m.Tasks.CreateTask(ctx, &model.Task{WorkflowID: wf.ID, UserID: u.ID, Input: "t"}); err != nil {
			t.Fatalf("CreateTask: %v", err)
		}
	}

	t.Run("limit=500 returns 100 rows (clamped)", func(t *testing.T) {
		got, err := m.Tasks.ListTasks(ctx, u.ID, 500, 0)
		if err != nil {
			t.Fatalf("ListTasks: %v", err)
		}
		if len(got) != 100 {
			t.Fatalf("期望钳到上限 100 条，实际 %d 条（若为 20 说明 limit 被静默重置）", len(got))
		}
	})

	t.Run("limit omitted defaults to 20", func(t *testing.T) {
		got, err := m.Tasks.ListTasks(ctx, u.ID, 0, 0)
		if err != nil {
			t.Fatalf("ListTasks: %v", err)
		}
		if len(got) != 20 {
			t.Fatalf("未指定 limit 期望默认 20 条，实际 %d 条", len(got))
		}
	})

	t.Run("offset still works after clamp", func(t *testing.T) {
		first, _ := m.Tasks.ListTasks(ctx, u.ID, 100, 0)
		second, _ := m.Tasks.ListTasks(ctx, u.ID, 100, 100)
		if len(second) != 10 {
			t.Fatalf("110 条任务、limit=100 时第二页期望 10 条，实际 %d", len(second))
		}
		if first[0].ID == second[0].ID {
			t.Fatal("两页第一条相同，offset 未生效")
		}
	})
}
