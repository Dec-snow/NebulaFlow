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
