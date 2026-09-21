package scheduler

import (
	"errors"
	"testing"

	"github.com/hoarfrost/nebulaflow/internal/model"
)

func n(key string, typ model.NodeType) model.WorkflowNode {
	return model.WorkflowNode{NodeKey: key, NodeType: typ}
}

func e(src, dst string) model.WorkflowEdge {
	return model.WorkflowEdge{SourceNode: src, TargetNode: dst}
}

// 线性：A → B → C
func TestBuildDAG_Linear(t *testing.T) {
	nodes := []model.WorkflowNode{n("A", model.NodeInput), n("B", model.NodeLLM), n("C", model.NodeOutput)}
	edges := []model.WorkflowEdge{e("A", "B"), e("B", "C")}
	dag, err := BuildDAG(nodes, edges)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(dag.Order) != 3 {
		t.Fatalf("expected 3 nodes in topo order, got %v", dag.Order)
	}
	if dag.Order[0] != "A" || dag.Order[2] != "C" {
		t.Fatalf("wrong order: %v", dag.Order)
	}
}

// 并行：A → B, A → C, B,C → D
func TestBuildDAG_Parallel(t *testing.T) {
	nodes := []model.WorkflowNode{n("A", model.NodeInput), n("B", model.NodeLLM), n("C", model.NodeRAG), n("D", model.NodeOutput)}
	edges := []model.WorkflowEdge{e("A", "B"), e("A", "C"), e("B", "D"), e("C", "D")}
	dag, err := BuildDAG(nodes, edges)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if dag.InDegree["B"] != 1 || dag.InDegree["C"] != 1 || dag.InDegree["D"] != 2 {
		t.Fatalf("wrong in-degrees: %v", dag.InDegree)
	}
	// A 之后 B/C 应同时就绪（并行性）
	inDegree := copyInDegree(dag.InDegree)
	done := map[string]bool{"A": true}
	for _, k := range dag.Edges["A"] {
		inDegree[k]--
	}
	ready := dag.ReadyNodes(inDegree, done)
	if !contains(ready, "B") || !contains(ready, "C") {
		t.Fatalf("expected B and C ready in parallel, got %v", ready)
	}
}

// 环：A → B → C → A 应报错
func TestBuildDAG_Cycle(t *testing.T) {
	nodes := []model.WorkflowNode{n("A", model.NodeInput), n("B", model.NodeLLM), n("C", model.NodeOutput)}
	edges := []model.WorkflowEdge{e("A", "B"), e("B", "C"), e("C", "A")}
	_, err := BuildDAG(nodes, edges)
	if !errors.Is(err, ErrCycle) {
		t.Fatalf("expected ErrCycle, got %v", err)
	}
}

// 自环也应报错
func TestBuildDAG_SelfLoop(t *testing.T) {
	nodes := []model.WorkflowNode{n("A", model.NodeInput)}
	edges := []model.WorkflowEdge{e("A", "A")}
	_, err := BuildDAG(nodes, edges)
	if !errors.Is(err, ErrCycle) {
		t.Fatalf("expected ErrCycle for self-loop, got %v", err)
	}
}

// 悬挂边应报错
func TestBuildDAG_DanglingEdge(t *testing.T) {
	nodes := []model.WorkflowNode{n("A", model.NodeInput)}
	edges := []model.WorkflowEdge{e("A", "ghost")}
	_, err := BuildDAG(nodes, edges)
	if !errors.Is(err, ErrDanglingEdge) {
		t.Fatalf("expected ErrDanglingEdge, got %v", err)
	}
}

// 空工作流应报错
func TestBuildDAG_Empty(t *testing.T) {
	_, err := BuildDAG(nil, nil)
	if !errors.Is(err, ErrEmptyWorkflow) {
		t.Fatalf("expected ErrEmptyWorkflow, got %v", err)
	}
}

// 多入度节点就绪条件：仅当前驱全部完成后入度归零
func TestBuildDAG_JoinGate(t *testing.T) {
	nodes := []model.WorkflowNode{n("A", model.NodeInput), n("B", model.NodeLLM), n("C", model.NodeLLM), n("D", model.NodeOutput)}
	edges := []model.WorkflowEdge{e("A", "B"), e("A", "C"), e("B", "D"), e("C", "D")}
	dag, _ := BuildDAG(nodes, edges)

	inDegree := copyInDegree(dag.InDegree)
	done := map[string]bool{"A": true}
	for _, k := range dag.Edges["A"] {
		inDegree[k]--
	}
	// 只有 B 完成，D 不应就绪
	done["B"] = true
	inDegree["D"]--
	if dag.InDegree["D"] != 2 {
		t.Fatal("setup error")
	}
	if inDegree["D"] != 1 {
		t.Fatalf("expected D indegree 1, got %d", inDegree["D"])
	}
	ready := dag.ReadyNodes(inDegree, done)
	if contains(ready, "D") {
		t.Fatalf("D should not be ready until both parents done, got %v", ready)
	}
	// C 完成后 D 就绪
	inDegree["D"]--
	ready = dag.ReadyNodes(inDegree, done)
	if !contains(ready, "D") {
		t.Fatalf("D should be ready after both parents done, got %v", ready)
	}
}

func copyInDegree(src map[string]int) map[string]int {
	out := make(map[string]int, len(src))
	for k, v := range src {
		out[k] = v
	}
	return out
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// 单节点工作流：只有 input，没有边也没有下游。
func TestBuildDAG_SingleNode(t *testing.T) {
	nodes := []model.WorkflowNode{n("A", model.NodeInput)}
	dag, err := BuildDAG(nodes, nil)
	if err != nil {
		t.Fatalf("single node should be valid: %v", err)
	}
	if len(dag.Order) != 1 || dag.Order[0] != "A" {
		t.Fatalf("expected [A], got %v", dag.Order)
	}
	if dag.InDegree["A"] != 0 {
		t.Fatalf("single node should have in-degree 0, got %d", dag.InDegree["A"])
	}
}

// 孤立节点：节点存在但没有任何边连接，应该仍出现在拓扑序中。
func TestBuildDAG_IsolatedNodes(t *testing.T) {
	nodes := []model.WorkflowNode{
		n("A", model.NodeInput),
		n("B", model.NodeLLM),
		n("C", model.NodeOutput),
	}
	edges := []model.WorkflowEdge{e("A", "B")}
	// C 是孤立节点
	dag, err := BuildDAG(nodes, edges)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(dag.Order) != 3 {
		t.Fatalf("expected 3 nodes in order, got %v", dag.Order)
	}
	if !contains(dag.Order, "C") {
		t.Fatalf("isolated node C should appear in order, got %v", dag.Order)
	}
}

// 条件分支节点：input → condition → (true-branch, false-branch) → output
// 验证条件节点能被正确识别和排序。
func TestBuildDAG_ConditionBranching(t *testing.T) {
	nodes := []model.WorkflowNode{
		n("in", model.NodeInput),
		n("cond", model.NodeCondition),
		n("true_branch", model.NodeLLM),
		n("false_branch", model.NodeLLM),
		n("out", model.NodeOutput),
	}
	edges := []model.WorkflowEdge{
		e("in", "cond"),
		e("cond", "true_branch"),
		e("cond", "false_branch"),
		e("true_branch", "out"),
		e("false_branch", "out"),
	}
	dag, err := BuildDAG(nodes, edges)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(dag.Order) != 5 {
		t.Fatalf("expected 5 nodes, got %v", dag.Order)
	}
	// in 必须在最前，out 必须在最后
	if dag.Order[0] != "in" {
		t.Fatalf("first node should be in, got %s", dag.Order[0])
	}
	if dag.Order[4] != "out" {
		t.Fatalf("last node should be out, got %s", dag.Order[4])
	}
	// cond 的入度应为 1，出度应为 2
	if dag.InDegree["cond"] != 1 {
		t.Fatalf("cond in-degree should be 1, got %d", dag.InDegree["cond"])
	}
	if len(dag.Edges["cond"]) != 2 {
		t.Fatalf("cond should have 2 outgoing edges, got %d", len(dag.Edges["cond"]))
	}
}

// 三层以上的深层 DAG：验证拓扑排序对多层依赖的正确性。
//   A → B → D → F
//     → C → E ↗
func TestBuildDAG_DeepDiamond(t *testing.T) {
	nodes := []model.WorkflowNode{
		n("A", model.NodeInput),
		n("B", model.NodeLLM),
		n("C", model.NodeLLM),
		n("D", model.NodeLLM),
		n("E", model.NodeLLM),
		n("F", model.NodeOutput),
	}
	edges := []model.WorkflowEdge{
		e("A", "B"), e("A", "C"),
		e("B", "D"), e("C", "E"),
		e("D", "F"), e("E", "F"),
	}
	dag, err := BuildDAG(nodes, edges)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// A 在前，F 在后
	if dag.Order[0] != "A" || dag.Order[len(dag.Order)-1] != "F" {
		t.Fatalf("wrong order: %v", dag.Order)
	}
	// B 必须在 D 前，C 必须在 E 前
	pos := map[string]int{}
	for i, k := range dag.Order {
		pos[k] = i
	}
	if pos["B"] >= pos["D"] {
		t.Fatalf("B should come before D, got order %v", dag.Order)
	}
	if pos["C"] >= pos["E"] {
		t.Fatalf("C should come before E, got order %v", dag.Order)
	}
	if pos["D"] >= pos["F"] {
		t.Fatalf("D should come before F, got order %v", dag.Order)
	}
}

// 没有 input 节点的工作流：不应报错（调度器会处理），
// 但所有入度为 0 的节点都应该是就绪节点。
func TestBuildDAG_NoInputNode(t *testing.T) {
	nodes := []model.WorkflowNode{
		n("A", model.NodeLLM),
		n("B", model.NodeOutput),
	}
	edges := []model.WorkflowEdge{e("A", "B")}
	dag, err := BuildDAG(nodes, edges)
	if err != nil {
		t.Fatalf("workflow without input node should still build: %v", err)
	}
	if dag.InDegree["A"] != 0 {
		t.Fatalf("A should have in-degree 0 (root node), got %d", dag.InDegree["A"])
	}
}

// ValidateNodeType 覆盖所有已知类型。
func TestValidateNodeType(t *testing.T) {
	cases := []struct {
		t    model.NodeType
		want bool
	}{
		{model.NodeInput, true},
		{model.NodeLLM, true},
		{model.NodeRAG, true},
		{model.NodeTool, true},
		{model.NodeCondition, true},
		{model.NodeOutput, true},
		{model.NodeType("unknown"), false},
		{model.NodeType(""), false},
		{model.NodeType("  "), false},
	}
	for _, tc := range cases {
		if got := ValidateNodeType(tc.t); got != tc.want {
			t.Errorf("ValidateNodeType(%q) = %v, want %v", tc.t, got, tc.want)
		}
	}
}
