// Package tool 实现 Agent 可调用的工具注册中心。
// 第一版提供：Calculator（表达式计算）、HTTP Request（获取网页/API）、
// Time（当前时间）、CodeRunner（受限 Python 求值）。
package tool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Tool 是工具统一抽象：节点执行时通过 name 找到工具并调用。
type Tool interface {
	Name() string
	Description() string
	// Call 接收字符串参数（JSON 或纯文本），返回字符串结果。
	Call(ctx context.Context, arg string) (string, error)
}

// SchemaProvider 是可选接口：工具用 JSON Schema 描述自己的入参。
//
// 做成可选接口而不是往 Tool 里加方法，是为了不破坏已有的工具实现
// （含测试里的 barrier / block / gate 等假工具）。没实现它的工具
// 会在 Descriptors() 里拿到一个宽松 schema —— 模型仍能调用，只是没有参数提示。
type SchemaProvider interface {
	Parameters() json.RawMessage
}

// ArgumentCaller 是可选接口：工具自己把 function calling 的 JSON 参数
// 翻译成它需要的入参。
//
// 为什么需要它：Call 的入参是一个裸字符串（用户在画布上就是这么填的），
// 而模型发来的是 JSON。若由中间层统一"猜"怎么拆，规则要么过宽（把多参工具的
// 参数压扁）要么过窄（模型换个键名就失效）。交给工具自己解释最准确。
type ArgumentCaller interface {
	CallWithArguments(ctx context.Context, args string) (string, error)
}

// ErrUnknownTool 表示请求的工具未注册。
var ErrUnknownTool = errors.New("unknown tool")

// Descriptor 是工具的自描述，用于生成 function calling 的 tools[]。
type Descriptor struct {
	Name        string
	Description string
	Parameters  json.RawMessage
}

// defaultParameters 是未声明 schema 时的兜底：接受任意对象、不要求任何字段。
// 不用 null —— 部分厂商会因此拒绝整个 tools 数组。
var defaultParameters = json.RawMessage(`{"type":"object","properties":{}}`)

type Registry struct {
	tools map[string]Tool
	order []string
}

func NewRegistry(tools ...Tool) *Registry {
	r := &Registry{tools: map[string]Tool{}}
	for _, t := range tools {
		r.Register(t)
	}
	return r
}

func (r *Registry) Register(t Tool) {
	if _, ok := r.tools[t.Name()]; !ok {
		r.order = append(r.order, t.Name())
	}
	r.tools[t.Name()] = t
}

func (r *Registry) Get(name string) (Tool, bool) {
	t, ok := r.tools[name]
	return t, ok
}

func (r *Registry) Names() []string { return r.order }

// Descriptors 返回全部工具的自描述，顺序与注册顺序一致。
// 顺序稳定是有意的：它决定模型看到的工具清单顺序，随机顺序会让
// 同一份工作流在不同进程里给出不同的调用倾向，压测与回归都无从对比。
func (r *Registry) Descriptors() []Descriptor {
	out, _ := r.DescriptorsFor(nil)
	return out
}

// DescriptorsFor 返回指定工具名的描述；names 为空表示"全部"。
// 第二个返回值是请求了但没注册的名字，交由调用方决定怎么提示 ——
// 这里不静默吞掉，因为"配置写错工具名"和"模型选择不调工具"是两回事，
// 前者需要人能看见。
func (r *Registry) DescriptorsFor(names []string) ([]Descriptor, []string) {
	pick := func(name string) (Descriptor, bool) {
		t, ok := r.tools[name]
		if !ok {
			return Descriptor{}, false
		}
		d := Descriptor{Name: t.Name(), Description: t.Description()}
		if sp, ok := t.(SchemaProvider); ok {
			d.Parameters = sp.Parameters()
		}
		if len(d.Parameters) == 0 {
			d.Parameters = defaultParameters
		}
		return d, true
	}

	if len(names) == 0 {
		out := make([]Descriptor, 0, len(r.order))
		for _, name := range r.order {
			if d, ok := pick(name); ok {
				out = append(out, d)
			}
		}
		return out, nil
	}

	out := make([]Descriptor, 0, len(names))
	var unknown []string
	seen := map[string]bool{}
	for _, name := range names {
		if seen[name] {
			continue
		}
		seen[name] = true
		if d, ok := pick(name); ok {
			out = append(out, d)
		} else {
			unknown = append(unknown, name)
		}
	}
	return out, unknown
}

// CallWithArguments 以 function calling 的 JSON 参数调用工具。
//
// 未实现 ArgumentCaller 的工具走兜底规则：参数是"恰好一个字段的 JSON 对象"
// 时取其值，否则原样透传。这条规则故意保守 —— 原样透传至少能让工具自己报错，
// 而错误地拆包会让工具收到一个语法正确但语义全错的入参。
func (r *Registry) CallWithArguments(ctx context.Context, name, args string) (string, error) {
	t, ok := r.Get(name)
	if !ok {
		return "", fmt.Errorf("%w: %s", ErrUnknownTool, name)
	}
	if ac, ok := t.(ArgumentCaller); ok {
		return ac.CallWithArguments(ctx, args)
	}
	if v, ok := unwrapSingleField(args); ok {
		return t.Call(ctx, v)
	}
	return t.Call(ctx, args)
}

// unwrapSingleField 在参数是"恰好一个字段的 JSON 对象"时取出该字段的值。
func unwrapSingleField(raw string) (string, bool) {
	s := strings.TrimSpace(raw)
	if !strings.HasPrefix(s, "{") {
		return "", false
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal([]byte(s), &obj); err != nil || len(obj) != 1 {
		return "", false
	}
	for _, v := range obj {
		return rawToString(v), true
	}
	return "", false
}

// argFromJSON 从 JSON 参数里按优先级取出一个字符串入参。
//
// 依次尝试：期望的键 → 对象里唯一的那个字段 → 裸 JSON 字符串 → 原样返回。
// 最后两级是为了兼容模型不按 schema 出牌的情况（直接给 "1+1" 而不是 {"expr":"1+1"}），
// 这在真实调用里很常见，直接报错会让 Agent 显得很脆。
func argFromJSON(raw string, keys ...string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal([]byte(s), &obj); err == nil {
		for _, k := range keys {
			if v, ok := obj[k]; ok {
				return rawToString(v)
			}
		}
		if len(obj) == 1 {
			for _, v := range obj {
				return rawToString(v)
			}
		}
		return ""
	}
	var str string
	if err := json.Unmarshal([]byte(s), &str); err == nil {
		return str
	}
	return s
}

func rawToString(raw json.RawMessage) string {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	return string(raw)
}

// ---------- Calculator ----------

var exprRe = regexp.MustCompile(`^[\d\s\+\-\*\/\(\)\.\%]+$`)

type Calculator struct{}

func (Calculator) Name() string        { return "calculator" }
func (Calculator) Description() string { return "计算数学表达式，如 (12+34)*5/2" }

func (Calculator) Parameters() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"expr":{"type":"string","description":"要计算的数学表达式，只允许数字与 + - * / ( ) % 字符"}},"required":["expr"]}`)
}

// CallWithArguments 接受 {"expr":"..."} 形式的入参（也兼容模型直接给表达式）。
func (Calculator) CallWithArguments(ctx context.Context, args string) (string, error) {
	return Calculator{}.Call(ctx, argFromJSON(args, "expr", "expression"))
}

// Call 通过 Go 自带求值实现安全的四则运算（不引入 eval 库）。
func (Calculator) Call(_ context.Context, arg string) (string, error) {
	arg = strings.TrimSpace(arg)
	if !exprRe.MatchString(arg) {
		return "", errors.New("calculator: 表达式包含不支持的字符")
	}
	v, err := evalExpr(arg)
	if err != nil {
		return "", fmt.Errorf("calculator: %w", err)
	}
	return strconv.FormatFloat(v, 'f', -1, 64), nil
}

// 递归下降求值器：仅支持 + - * / ( ) 和数字
func evalExpr(s string) (float64, error) {
	p := &parser{s: s}
	v, err := p.parseExpr()
	if err != nil {
		return 0, err
	}
	if p.pos < len(p.s) {
		return 0, errors.New("多余字符")
	}
	return v, nil
}

type parser struct {
	s   string
	pos int
}

func (p *parser) skipWS() {
	for p.pos < len(p.s) && (p.s[p.pos] == ' ' || p.s[p.pos] == '\t') {
		p.pos++
	}
}

func (p *parser) parseExpr() (float64, error) {
	v, err := p.parseTerm()
	if err != nil {
		return 0, err
	}
	for {
		p.skipWS()
		if p.pos >= len(p.s) {
			break
		}
		switch p.s[p.pos] {
		case '+':
			p.pos++
			r, err := p.parseTerm()
			if err != nil {
				return 0, err
			}
			v += r
		case '-':
			p.pos++
			r, err := p.parseTerm()
			if err != nil {
				return 0, err
			}
			v -= r
		default:
			return v, nil
		}
	}
	return v, nil
}

func (p *parser) parseTerm() (float64, error) {
	v, err := p.parseFactor()
	if err != nil {
		return 0, err
	}
	for {
		p.skipWS()
		if p.pos >= len(p.s) {
			break
		}
		switch p.s[p.pos] {
		case '*':
			p.pos++
			r, err := p.parseFactor()
			if err != nil {
				return 0, err
			}
			v *= r
		case '/':
			p.pos++
			r, err := p.parseFactor()
			if err != nil {
				return 0, err
			}
			if r == 0 {
				return 0, errors.New("除零")
			}
			v /= r
		default:
			return v, nil
		}
	}
	return v, nil
}

func (p *parser) parseFactor() (float64, error) {
	p.skipWS()
	if p.pos >= len(p.s) {
		return 0, errors.New("表达式不完整")
	}
	if p.s[p.pos] == '(' {
		p.pos++
		v, err := p.parseExpr()
		if err != nil {
			return 0, err
		}
		p.skipWS()
		if p.pos >= len(p.s) || p.s[p.pos] != ')' {
			return 0, errors.New("缺少右括号")
		}
		p.pos++
		return v, nil
	}
	if p.s[p.pos] == '-' {
		p.pos++
		v, err := p.parseFactor()
		if err != nil {
			return 0, err
		}
		return -v, nil
	}
	start := p.pos
	for p.pos < len(p.s) && (p.s[p.pos] >= '0' && p.s[p.pos] <= '9' || p.s[p.pos] == '.') {
		p.pos++
	}
	if start == p.pos {
		return 0, errors.New("无效数字")
	}
	return strconv.ParseFloat(p.s[start:p.pos], 64)
}

// ---------- HTTP Request ----------

type HTTPTool struct {
	client *http.Client
}

func NewHTTPTool() *HTTPTool {
	return &HTTPTool{client: &http.Client{Timeout: 15 * time.Second}}
}

func (t *HTTPTool) Name() string { return "http_request" }
func (t *HTTPTool) Description() string {
	return "请求一个 URL 并返回正文（截断到 4000 字符）"
}

func (t *HTTPTool) Parameters() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"url":{"type":"string","description":"要请求的完整 URL"},"method":{"type":"string","enum":["GET","POST"],"description":"HTTP 方法，默认 GET"}},"required":["url"]}`)
}

// CallWithArguments 直接把 JSON 交给 Call —— HTTPTool 的 Call 本来
// 就按 {"url","method"} 解析，无需额外翻译。
func (t *HTTPTool) CallWithArguments(ctx context.Context, args string) (string, error) {
	return t.Call(ctx, args)
}

func (t *HTTPTool) Call(ctx context.Context, arg string) (string, error) {
	var req struct {
		URL    string `json:"url"`
		Method string `json:"method"`
	}
	if err := json.Unmarshal([]byte(arg), &req); err != nil || req.URL == "" {
		// 兼容纯 URL 传参
		req.URL = strings.Trim(strings.TrimSpace(arg), `"`)
		if req.URL == "" {
			return "", errors.New("http_request: 需要 url")
		}
		req.Method = "GET"
	}
	httpReq, err := http.NewRequestWithContext(ctx, req.Method, req.URL, nil)
	if err != nil {
		return "", err
	}
	httpReq.Header.Set("User-Agent", "NebulaFlow-Agent/1.0")
	resp, err := t.client.Do(httpReq)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4000))
	if resp.StatusCode >= 400 {
		return fmt.Sprintf("HTTP %d: %s", resp.StatusCode, string(body)), nil
	}
	return string(body), nil
}

// ---------- Time ----------

type TimeTool struct{}

func (TimeTool) Name() string        { return "time" }
func (TimeTool) Description() string { return "返回当前时间" }

func (TimeTool) Parameters() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{}}`)
}

func (TimeTool) CallWithArguments(_ context.Context, _ string) (string, error) {
	return time.Now().Format(time.RFC3339), nil
}

func (TimeTool) Call(_ context.Context, _ string) (string, error) {
	return time.Now().Format(time.RFC3339), nil
}

// ---------- Code Runner（受限 Python 求值） ----------

// CodeRunner 用 Go 内置表达式求值代替真实 Python sandbox：
// 支持算术表达式与简单统计（min/max/avg/sum），
// 生产环境可替换为 gVisor/Docker 隔离的 Python 执行器。
type CodeRunner struct{}

func (CodeRunner) Name() string { return "code_runner" }
func (CodeRunner) Description() string {
	return "执行简单统计表达式：sum([1,2,3]) / avg([...]) / max([...]) / min([...])"
}

func (CodeRunner) Parameters() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"expr":{"type":"string","description":"统计表达式，如 sum([1,2,3])、avg([10,20])"}},"required":["expr"]}`)
}

func (CodeRunner) CallWithArguments(ctx context.Context, args string) (string, error) {
	return CodeRunner{}.Call(ctx, argFromJSON(args, "expr", "expression", "code"))
}

var listRe = regexp.MustCompile(`^(sum|avg|max|min)\(\[([\d\s,\.]*)\]\)$`)

func (CodeRunner) Call(_ context.Context, arg string) (string, error) {
	arg = strings.TrimSpace(arg)
	m := listRe.FindStringSubmatch(arg)
	if m == nil {
		return "", errors.New("code_runner: 仅支持 sum/avg/max/min 列表操作")
	}
	var nums []float64
	for _, part := range strings.Split(m[2], ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		v, err := strconv.ParseFloat(part, 64)
		if err != nil {
			return "", err
		}
		nums = append(nums, v)
	}
	if len(nums) == 0 {
		return "", errors.New("code_runner: 空列表")
	}
	var out float64
	switch m[1] {
	case "sum":
		for _, v := range nums {
			out += v
		}
	case "avg":
		for _, v := range nums {
			out += v
		}
		out /= float64(len(nums))
	case "max":
		out = nums[0]
		for _, v := range nums[1:] {
			if v > out {
				out = v
			}
		}
	case "min":
		out = nums[0]
		for _, v := range nums[1:] {
			if v < out {
				out = v
			}
		}
	}
	return strconv.FormatFloat(out, 'f', -1, 64), nil
}
