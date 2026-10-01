package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// KnowledgeTool 把 RAG 知识库检索封装成 Agent 可调用的工具。
//
// 这是实现 Agentic RAG 的关键：不是"每次提问都先检索再回答"（普通 RAG），
// 而是让 Agent 自己判断"这个问题需不需要查知识库"，需要就调，
// 不需要就直接答——这才是真正的 Agent 式检索。
//
// 对比：
//   普通 RAG：问题 → 检索 → LLM 回答  （永远检索，可能检索了没用的内容）
//   Agentic RAG：问题 → LLM 判断 → （可选）检索 → LLM 回答
//
// 后者更智能，也更省 token（不需要每次都塞一堆上下文）。
type KnowledgeTool struct {
	// SearchFunc 是实际的检索函数（由调用方注入，避免循环依赖）。
	// 输入：知识库 ID + 查询文本 + Top-K；输出：检索到的文本块列表。
	SearchFunc func(ctx context.Context, kbID int64, query string, topK int) ([]SearchHit, error)
	// DefaultKbID 是默认知识库 ID（参数里没指定时用这个）。
	DefaultKbID int64
	// DefaultTopK 是默认返回的块数。
	DefaultTopK int
}

// SearchHit 是检索结果（和 rag 包的 DocumentChunk 解耦，tool 包不直接依赖 rag）。
type SearchHit struct {
	Source   string  // 来源文档名
	Content  string  // 文本内容
	Score    float64 // 相似度分数
}

func (t *KnowledgeTool) Name() string { return "search_knowledge" }

func (t *KnowledgeTool) Description() string {
	return "搜索企业知识库，返回相关的文档片段。当问题需要内部资料、产品文档、政策法规等知识库内容时使用。"
}

func (t *KnowledgeTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "query": {
      "type": "string",
      "description": "要检索的问题或关键词"
    },
    "knowledge_base_id": {
      "type": "integer",
      "description": "知识库 ID，默认为 1"
    },
    "top_k": {
      "type": "integer",
      "description": "返回最相关的片段数量，默认 3"
    }
  },
  "required": ["query"]
}`)
}

// CallWithArguments 以 function calling 的 JSON 参数调用检索工具。
func (t *KnowledgeTool) CallWithArguments(ctx context.Context, args string) (string, error) {
	query := argFromJSON(args, "query", "question", "q")
	if strings.TrimSpace(query) == "" {
		return "", fmt.Errorf("search_knowledge: 需要 query 参数")
	}
	kbID := t.DefaultKbID
	if kbID == 0 {
		kbID = 1
	}
	if v, ok := jsonIntField(args, "knowledge_base_id", "kb_id", "kbId"); ok && v > 0 {
		kbID = int64(v)
	}
	topK := t.DefaultTopK
	if topK == 0 {
		topK = 3
	}
	if v, ok := jsonIntField(args, "top_k", "k", "topK"); ok && v > 0 {
		topK = v
	}
	return t.Call(ctx, fmt.Sprintf("%d:%d:%s", kbID, topK, query))
}

// Call 接收 "kbID:topK:query" 格式的字符串（兼容非 Agent 场景的直接调用）。
func (t *KnowledgeTool) Call(ctx context.Context, arg string) (string, error) {
	if t.SearchFunc == nil {
		return "", fmt.Errorf("search_knowledge: 检索函数未配置")
	}
	// 解析参数：兼容 "kbID:topK:query" 和纯 query 两种格式
	kbID := t.DefaultKbID
	if kbID == 0 {
		kbID = 1
	}
	topK := t.DefaultTopK
	if topK == 0 {
		topK = 3
	}
	query := arg
	if parts := strings.SplitN(arg, ":", 3); len(parts) == 3 {
		if id, err := strconv.ParseInt(parts[0], 10, 64); err == nil && id > 0 {
			kbID = id
		}
		if k, err := strconv.Atoi(parts[1]); err == nil && k > 0 {
			topK = k
		}
		query = parts[2]
	}

	hits, err := t.SearchFunc(ctx, kbID, query, topK)
	if err != nil {
		return "", fmt.Errorf("search_knowledge: %w", err)
	}
	if len(hits) == 0 {
		return "未检索到相关内容。", nil
	}

	// 格式化输出：编号 + 来源 + 内容
	var sb strings.Builder
	fmt.Fprintf(&sb, "检索到 %d 条相关内容：\n\n", len(hits))
	for i, h := range hits {
		fmt.Fprintf(&sb, "【%d】来源：%s（相似度：%.2f）\n%s\n\n",
			i+1, h.Source, h.Score, h.Content)
	}
	return sb.String(), nil
}

// jsonIntField 从 JSON 参数里取整数字段（兼容多种键名）。
func jsonIntField(raw string, keys ...string) (int, bool) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return 0, false
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal([]byte(s), &obj); err != nil {
		return 0, false
	}
	for _, k := range keys {
		if v, ok := obj[k]; ok {
			var n int
			if err := json.Unmarshal(v, &n); err == nil {
				return n, true
			}
			// 试试 float64（JSON 数字默认是 float）
			var f float64
			if err := json.Unmarshal(v, &f); err == nil {
				return int(f), true
			}
		}
	}
	return 0, false
}
