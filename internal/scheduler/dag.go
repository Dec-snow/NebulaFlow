// Package scheduler 实现 NebulaFlow 的核心：DAG 工作流调度引擎。
//
// 职责：
//  1. 从任务队列消费任务；
//  2. 将 Workflow 解析为 DAG，计算依赖与拓扑序；
//  3. 就绪节点（入度 0）提交 Worker Pool 并发执行；
//  4. 节点完成后更新依赖计数，逐层推进，直至全部完成；
//  5. 超时 / 取消 / 重试统一在此层处理；
//  6. 所有状态变化通过 task.Hub 推送给 SSE 客户端。
//
// 面试要点：拓扑排序 + 并发调度 + 背压 + 优雅取消。
package scheduler

import (
	"errors"
	"fmt"
	"sort"

	"github.com/hoarfrost/nebulaflow/internal/model"
)

// Node 是 DAG 中的顶点（workflow_node 的轻量视图）。
type Node struct {
	Key    string
	Type   model.NodeType
	Config model.NodeConfig
	ID     int64
}

// DAG 是工作流的有向无环图表示。
type DAG struct {
	Nodes    map[string]*Node
	Edges    map[string][]string // source → targets
	InDegree map[string]int
	Order    []string // 拓扑序（环检测失败时为空）
}

var (
	ErrEmptyWorkflow = errors.New("workflow has no nodes")
	ErrCycle         = errors.New("workflow graph contains a cycle")
	ErrDanglingEdge  = errors.New("workflow edge references unknown node")
	ErrDuplicateNode = errors.New("workflow contains duplicate node_key")
	ErrInvalidNode   = errors.New("workflow node is invalid")
)

// ValidateNodeType 判断节点类型是否是引擎支持的类型。
// API 层在保存工作流时调用，避免把无法执行的节点写进库里。
func ValidateNodeType(t model.NodeType) bool {
	switch t {
	case model.NodeInput, model.NodeLLM, model.NodeRAG, model.NodeTool, model.NodeOutput:
		return true
	default:
		return false
	}
}

// BuildDAG 从 workflow 的节点/边构建 DAG 并做合法性校验：
//   - 节点非空、node_key 唯一且非空、node_type 必须是引擎支持的类型
//   - 边两端节点必须存在
//   - 无环（Kahn 拓扑排序）
//
// 拓扑序在节点 key 上做字典序 tie-break，保证同一 workflow 每次执行
// 的遍历顺序一致（日志/回放可复现），而不是依赖 Go map 的随机迭代顺序。
func BuildDAG(nodes []model.WorkflowNode, edges []model.WorkflowEdge) (*DAG, error) {
	if len(nodes) == 0 {
		return nil, ErrEmptyWorkflow
	}
	dag := &DAG{
		Nodes:    make(map[string]*Node, len(nodes)),
		Edges:    make(map[string][]string, len(nodes)),
		InDegree: make(map[string]int, len(nodes)),
	}
	for i := range nodes {
		n := nodes[i]
		if n.NodeKey == "" {
			return nil, fmt.Errorf("%w: empty node_key at index %d", ErrInvalidNode, i)
		}
		if _, dup := dag.Nodes[n.NodeKey]; dup {
			return nil, fmt.Errorf("%w: %q", ErrDuplicateNode, n.NodeKey)
		}
		if !ValidateNodeType(n.NodeType) {
			return nil, fmt.Errorf("%w: node %q has unsupported type %q", ErrInvalidNode, n.NodeKey, n.NodeType)
		}
		dag.Nodes[n.NodeKey] = &Node{
			Key:    n.NodeKey,
			Type:   n.NodeType,
			Config: n.Config,
			ID:     n.ID,
		}
		dag.InDegree[n.NodeKey] = 0
	}
	seen := make(map[[2]string]struct{}, len(edges))
	for _, e := range edges {
		if _, ok := dag.Nodes[e.SourceNode]; !ok {
			return nil, fmt.Errorf("%w: %s", ErrDanglingEdge, e.SourceNode)
		}
		if _, ok := dag.Nodes[e.TargetNode]; !ok {
			return nil, fmt.Errorf("%w: %s", ErrDanglingEdge, e.TargetNode)
		}
		if e.SourceNode == e.TargetNode {
			return nil, fmt.Errorf("%w: self-loop %s", ErrCycle, e.SourceNode)
		}
		pair := [2]string{e.SourceNode, e.TargetNode}
		if _, dup := seen[pair]; dup {
			// 重复边会让入度虚高，节点永远等不到所有前驱完成。
			return nil, fmt.Errorf("%w: duplicate edge %s->%s", ErrInvalidNode, e.SourceNode, e.TargetNode)
		}
		seen[pair] = struct{}{}
		dag.Edges[e.SourceNode] = append(dag.Edges[e.SourceNode], e.TargetNode)
		dag.InDegree[e.TargetNode]++
	}

	// Kahn 拓扑排序（同时检测环）。
	// 注意：用 inDegree 副本排序，保留 dag.InDegree 的原始值供调度使用。
	inDegree := make(map[string]int, len(dag.InDegree))
	for k, v := range dag.InDegree {
		inDegree[k] = v
	}
	queue := make([]string, 0, len(dag.Nodes))
	for k, d := range inDegree {
		if d == 0 {
			queue = append(queue, k)
		}
	}
	sort.Strings(queue) // 确定性：同层节点按 key 字典序出队
	visited := 0
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		dag.Order = append(dag.Order, cur)
		visited++
		// 每层的后继同样排序，保证拓扑序稳定
		next := make([]string, 0, len(dag.Edges[cur]))
		for _, t := range dag.Edges[cur] {
			inDegree[t]--
			if inDegree[t] == 0 {
				next = append(next, t)
			}
		}
		if len(next) > 1 {
			sort.Strings(next)
		}
		queue = append(queue, next...)
	}
	if visited != len(dag.Nodes) {
		return nil, ErrCycle
	}
	return dag, nil
}

// ReadyNodes 返回当前入度为 0 且未完成的节点（并行的自然体现）。
// 结果按 key 字典序排序：调度顺序稳定，日志与回放可复现。
func (d *DAG) ReadyNodes(inDegree map[string]int, done map[string]bool) []string {
	var out []string
	for k := range d.Nodes {
		if !done[k] && inDegree[k] == 0 {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// InputsOf 返回某节点的直接前驱 key 列表。
// 顺序固定为该节点入边的定义顺序（DAG 构建时建立），保证多依赖拼接出的
// prompt 在多次执行间完全一致——否则同一次输入可能得到不同的 LLM 结果。
func (d *DAG) InputsOf(key string) []string {
	var out []string
	for src, targets := range d.Edges {
		for _, t := range targets {
			if t == key {
				out = append(out, src)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		// 先按入边在 Edges[src] 中的下标排序，再从 src 字典序兜底
		return out[i] < out[j]
	})
	return out
}
