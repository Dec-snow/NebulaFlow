package main

import (
	"testing"

	"github.com/hoarfrost/nebulaflow/internal/model"
)

// TestDemoRoleFor 覆盖 seed 的 admin 引导规则。
//
// 背景：POST/PUT/DELETE /api/providers 是 admin-only，而"第一个注册用户自动成为
// admin"的规则在先跑过 seed 的库上永远不会触发（注册时 users 表里已有 demo）。
// 结果是全新部署的库可能一个 admin 都没有 —— 通过 API 配真实模型 Provider 直接
// 403 insufficient permission。seed 因此必须在"零 admin"的库上补一个 admin，
// 但绝不能在已有 admin 的库上夺权或重置别人手工调过的角色。
func TestDemoRoleFor(t *testing.T) {
	cases := []struct {
		name        string
		adminCount  int64
		currentRole string
		want        string
	}{
		{"零 admin + 普通用户 → 提升为 admin", 0, model.RoleUser, model.RoleAdmin},
		{"零 admin + 空角色 → 提升为 admin", 0, "", model.RoleAdmin},
		{"零 admin + 已是 admin → 保持 admin", 0, model.RoleAdmin, model.RoleAdmin},
		{"已有 admin + 普通用户 → 不动角色", 1, model.RoleUser, model.RoleUser},
		{"已有 admin + 已是 admin → 保持", 3, model.RoleAdmin, model.RoleAdmin},
		{"已有 admin + 自定义角色 → 不动角色", 2, "auditor", "auditor"},
	}
	for _, c := range cases {
		if got := demoRoleFor(c.adminCount, c.currentRole); got != c.want {
			t.Errorf("%s: demoRoleFor(%d, %q) = %q，期望 %q",
				c.name, c.adminCount, c.currentRole, got, c.want)
		}
	}
}
