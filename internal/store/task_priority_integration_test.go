package store

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/hoarfrost/nebulaflow/internal/model"
)

// TestTaskPriority_CreateGetList_RoundTrip 覆盖"PG 端任务优先级被丢弃"这条缺陷。
//
// 列是真存在的（006_priority_retry.sql：priority VARCHAR(16) NOT NULL DEFAULT 'normal'），
// 内存实现也保留它，单测因此一直是绿的；但 PG 实现里：
//   - CreateTask 的 INSERT 完全没提 priority → 无论调用方传什么，库里都是默认 'normal'；
//   - GetTask / ListTasks / GetTaskByID 的 SELECT 也没有它 → 读回来永远是零值 ""。
//
// 后果：优先级在 PG 模式下形同虚设，而"内存模式对、PG 模式错"正是最难发现的一类漂移。
func TestTaskPriority_CreateGetList_RoundTrip(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	db := pgTestDB(t, ctx, 384)
	repo := &TaskRepo{db: db}
	userID := pgTestUserID(t, ctx, db, "task-priority")

	// tasks.workflow_id 是 NOT NULL + FK，先准备一个工作流。
	var wfID int64
	if err := db.Pool.QueryRow(ctx,
		`INSERT INTO workflows (user_id, name, description)
		 VALUES ($1, 'store-int-task-priority', '') RETURNING id`, userID).Scan(&wfID); err != nil {
		t.Fatalf("准备测试工作流: %v", err)
	}
	t.Cleanup(func() {
		// 级联删除 tasks（tasks.workflow_id → workflows.id ON DELETE CASCADE）
		if _, err := db.Pool.Exec(context.Background(),
			`DELETE FROM workflows WHERE id=$1`, wfID); err != nil {
			t.Logf("清理测试工作流 %d 失败: %v", wfID, err)
		}
	})

	cases := []struct {
		name string
		in   string
		want string
	}{
		{"high 原样往返", model.PriorityHigh, model.PriorityHigh},
		{"low 原样往返", model.PriorityLow, model.PriorityLow},
		{"normal 原样往返", model.PriorityNormal, model.PriorityNormal},
		{"空值兜底为 normal", "", model.PriorityNormal},
		{"非法值兜底为 normal", "URGENT", model.PriorityNormal},
	}

	type made struct {
		id    int64
		name  string
		want  string
		gotAt string // 首次发现不符的环节
	}
	created := make([]made, 0, len(cases))

	// 不在这里立刻 Fatal：先把"写 / 读 / 列 / 落库"四个环节的偏差都收齐再一起报，
	// 这样一次运行就能看出优先级是在哪一环丢的。
	var bad []string

	for i, c := range cases {
		task := &model.Task{
			WorkflowID: wfID, UserID: userID, Status: model.TaskPending,
			Input: fmt.Sprintf("priority case %d", i), Priority: c.in,
		}
		if err := repo.CreateTask(ctx, task); err != nil {
			t.Fatalf("CreateTask(priority=%q): %v", c.in, err)
		}
		m := made{id: task.ID, name: c.name, want: c.want}
		if task.Priority != c.want {
			bad = append(bad, fmt.Sprintf("CreateTask(%s) 回填 priority=%q，期望 %q", c.name, task.Priority, c.want))
			m.gotAt = "CreateTask"
		}
		created = append(created, m)
	}

	// ---- GetTask：修复前这里恒为零值 "" ----
	for i, m := range created {
		got, err := repo.GetTask(ctx, m.id, userID)
		if err != nil {
			t.Fatalf("GetTask(%d): %v", m.id, err)
		}
		if got.Priority != m.want {
			bad = append(bad, fmt.Sprintf("GetTask(%s).Priority=%q，期望 %q", m.name, got.Priority, m.want))
			if created[i].gotAt == "" {
				created[i].gotAt = "GetTask"
			}
		}
	}

	// ---- GetTaskByID：调度器读任务的路径，同样不能丢优先级 ----
	for i, m := range created {
		got, err := repo.GetTaskByID(ctx, m.id)
		if err != nil {
			t.Fatalf("GetTaskByID(%d): %v", m.id, err)
		}
		if got.Priority != m.want {
			bad = append(bad, fmt.Sprintf("GetTaskByID(%s).Priority=%q，期望 %q", m.name, got.Priority, m.want))
			if created[i].gotAt == "" {
				created[i].gotAt = "GetTaskByID"
			}
		}
	}

	// ---- ListTasks：修复前同样恒为零值 ----
	list, err := repo.ListTasks(ctx, userID, 100, 0)
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	byID := make(map[int64]string, len(list))
	for _, tk := range list {
		byID[tk.ID] = tk.Priority
	}
	for i, m := range created {
		p, ok := byID[m.id]
		if !ok {
			bad = append(bad, fmt.Sprintf("ListTasks 未返回任务 %d", m.id))
			continue
		}
		if p != m.want {
			bad = append(bad, fmt.Sprintf("ListTasks(%s).Priority=%q，期望 %q", m.name, p, m.want))
			if created[i].gotAt == "" {
				created[i].gotAt = "ListTasks"
			}
		}
	}

	// ---- 落库真值：直接查库确认 PG 里存的也是归一化后的值 ----
	for i, m := range created {
		var raw string
		if err := db.Pool.QueryRow(ctx,
			`SELECT priority FROM tasks WHERE id=$1`, m.id).Scan(&raw); err != nil {
			t.Fatalf("直接查库读取任务 %d 的 priority: %v", m.id, err)
		}
		if raw != m.want {
			bad = append(bad, fmt.Sprintf("tasks.priority 落库值(%s)=%q，期望 %q", m.name, raw, m.want))
			if created[i].gotAt == "" {
				created[i].gotAt = "直接查库"
			}
		}
	}

	if len(bad) > 0 {
		t.Fatalf("优先级在 PG 模式下丢了（修复前 priority 既没进 INSERT/RETURNING，"+
			"也没进 GetTask/GetTaskByID/ListTasks 的 SELECT）：\n  - %s", strings.Join(bad, "\n  - "))
	}
}
