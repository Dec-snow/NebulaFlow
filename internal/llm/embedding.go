package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"io"
	"math"
	"net/http"
	"strings"
	"time"
)

// Embedder 是向量化抽象：同一接口对接远端 Embedding 服务与本地降级实现。
type Embedder interface {
	Name() string
	Embed(ctx context.Context, texts []string) ([][]float64, error)
}

// ---------- OpenAI 兼容 Embedding（OpenAI / 其他兼容厂商） ----------

type OpenAICompatEmbedder struct {
	name    string
	baseURL string
	apiKey  string
	model   string
	client  *http.Client
}

func NewOpenAIEmbedder(baseURL, apiKey, model string) *OpenAICompatEmbedder {
	if baseURL == "" {
		baseURL = "https://api.openai.com/v1"
	}
	if model == "" {
		model = "text-embedding-3-small"
	}
	return &OpenAICompatEmbedder{name: "openai-embed", baseURL: strings.TrimRight(baseURL, "/"),
		apiKey: apiKey, model: model, client: &http.Client{Timeout: 60 * time.Second}}
}

// ---------- Ollama Embedding（/api/embed） ----------

type ollamaEmbedBody struct {
	Model string   `json:"model"`
	Input []string `json:"input"`
}
type ollamaEmbedResp struct {
	Embeddings [][]float64 `json:"embeddings"`
	Error      string      `json:"error"`
}

type OllamaEmbedder struct {
	name    string
	baseURL string
	model   string
	client  *http.Client
}

func NewOllamaEmbedder(baseURL, model string) *OllamaEmbedder {
	if baseURL == "" {
		baseURL = "http://localhost:11434"
	}
	if model == "" {
		model = "nomic-embed-text"
	}
	return &OllamaEmbedder{name: "ollama-embed", baseURL: strings.TrimRight(baseURL, "/"),
		model: model, client: &http.Client{Timeout: 60 * time.Second}}
}

// ---------- 本地降级实现（确定性 hash 词袋向量） ----------

// LocalHashEmbedder 是不依赖外部服务的确定性向量器：
// 对文本做小写分词，用 FNV hash 映射到固定维度词袋向量并做 L2 归一化。
// 用于离线演示、CI 测试与 Embedding 服务不可用时的降级。
type LocalHashEmbedder struct{ dim int }

func NewLocalHashEmbedder(dim int) *LocalHashEmbedder {
	if dim <= 0 {
		dim = 384
	}
	return &LocalHashEmbedder{dim: dim}
}

func (e *LocalHashEmbedder) Name() string { return "local-hash" }

func (e *LocalHashEmbedder) Embed(ctx context.Context, texts []string) ([][]float64, error) {
	out := make([][]float64, 0, len(texts))
	for _, t := range texts {
		vec := make([]float64, e.dim)
		for _, tok := range tokenize(t) {
			h := fnv.New32a()
			h.Write([]byte(tok))
			idx := int(h.Sum32()) % e.dim
			vec[idx]++
		}
		normalize(vec)
		out = append(out, vec)
	}
	return out, nil
}

// ---------- Embedding Gateway（带降级） ----------

type EmbeddingGateway struct {
	primary  Embedder // 远端服务（OpenAI / Ollama）
	fallback Embedder // 本地降级
}

func NewEmbeddingGateway(primary Embedder, fallback Embedder) *EmbeddingGateway {
	if fallback == nil {
		fallback = NewLocalHashEmbedder(0)
	}
	return &EmbeddingGateway{primary: primary, fallback: fallback}
}

// Embed 优先使用远端服务，失败/超时自动降级到本地实现。
func (g *EmbeddingGateway) Embed(ctx context.Context, texts []string) ([][]float64, error) {
	if g.primary != nil {
		vecs, err := g.primary.Embed(ctx, texts)
		if err == nil {
			return vecs, nil
		}
	}
	return g.fallback.Embed(ctx, texts)
}

// ---------- 相似度 ----------

// Cosine 计算余弦相似度（向量应已归一化）。
func Cosine(a, b []float64) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot float64
	for i := range a {
		dot += a[i] * b[i]
	}
	norm := func(v []float64) float64 {
		var s float64
		for _, x := range v {
			s += x * x
		}
		return math.Sqrt(s)
	}
	na, nb := norm(a), norm(b)
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (na * nb)
}

func normalize(v []float64) {
	var s float64
	for _, x := range v {
		s += x * x
	}
	if s == 0 {
		return
	}
	n := math.Sqrt(s)
	for i := range v {
		v[i] /= n
	}
}

func tokenize(s string) []string {
	var out []string
	for _, f := range strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r > 127)
	}) {
		runes := []rune(f)
		switch {
		case len(runes) == 1:
			out = append(out, f)
		case isASCIIWord(f):
			out = append(out, f)
		default:
			// 中文/多字节文本：按 bigram 切分，使检索能共享局部语义
			for i := 0; i < len(runes)-1; i++ {
				out = append(out, string(runes[i:i+2]))
			}
		}
	}
	return out
}

func isASCIIWord(s string) bool {
	for _, r := range s {
		if r > 127 {
			return false
		}
	}
	return true
}

// OpenAI 兼容 Embedding 实现
func (e *OpenAICompatEmbedder) Name() string { return e.name }

func (e *OpenAICompatEmbedder) Embed(ctx context.Context, texts []string) ([][]float64, error) {
	body := map[string]any{"model": e.model, "input": texts}
	payload, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.baseURL+"/embeddings", bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+e.apiKey)
	resp, err := e.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("openai embed: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("openai embed http %d: %s", resp.StatusCode, truncate(string(raw), 200))
	}
	var parsed struct {
		Data []struct {
			Embedding []float64 `json:"embedding"`
		} `json:"data"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, err
	}
	if parsed.Error != nil {
		return nil, fmt.Errorf("openai embed: %s", parsed.Error.Message)
	}
	out := make([][]float64, 0, len(parsed.Data))
	for _, d := range parsed.Data {
		normalize(d.Embedding)
		out = append(out, d.Embedding)
	}
	return out, nil
}

func (e *OllamaEmbedder) Name() string { return e.name }

func (e *OllamaEmbedder) Embed(ctx context.Context, texts []string) ([][]float64, error) {
	body := ollamaEmbedBody{Model: e.model, Input: texts}
	payload, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.baseURL+"/api/embed", bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := e.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("ollama embed: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("ollama embed http %d: %s", resp.StatusCode, truncate(string(raw), 200))
	}
	var parsed ollamaEmbedResp
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, err
	}
	if parsed.Error != "" {
		return nil, fmt.Errorf("ollama embed: %s", parsed.Error)
	}
	for _, v := range parsed.Embeddings {
		normalize(v)
	}
	return parsed.Embeddings, nil
}
