package tool

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// 本文件覆盖 P2-11 为 function calling 新增的两个可选接口：
// SchemaProvider（工具自述参数形状）与 ArgumentCaller（工具自己翻译 JSON 入参）。
// 原有的 registry_test.go 覆盖的是 Call 本身的行为，两者互补。

// plainTool 故意**不实现** SchemaProvider / ArgumentCaller，
// 用来验证"第三方工具什么都没实现"时注册表的兜底行为。
// 它记录每次收到的入参，方便断言解包规则是否按预期生效。
type plainTool struct {
	name string
	got  []string
}

func (p *plainTool) Name() string        { return p.name }
func (p *plainTool) Description() string { return "测试用：不声明 schema 的裸工具" }
func (p *plainTool) Call(_ context.Context, arg string) (string, error) {
	p.got = append(p.got, arg)
	return "got:" + arg, nil
}

// ---------- Descriptors ----------

func TestRegistryDescriptorsShape(t *testing.T) {
	reg := NewRegistry(Calculator{}, NewHTTPTool(), TimeTool{}, CodeRunner{})
	descs := reg.Descriptors()

	if len(descs) != 4 {
		t.Fatalf("工具数 = %d，期望 4", len(descs))
	}
	// 顺序必须与注册顺序一致：它决定模型看到的工具清单顺序，
	// 不稳定的话同一份工作流在不同进程里会有不同的调用倾向，无法回归对比。
	wantOrder := []string{"calculator", "http_request", "time", "code_runner"}
	for i, w := range wantOrder {
		if descs[i].Name != w {
			t.Errorf("descs[%d].Name = %q，期望 %q", i, descs[i].Name, w)
		}
		if descs[i].Description == "" {
			t.Errorf("%s 缺少描述：模型完全靠描述决定要不要调用它", descs[i].Name)
		}
		if len(descs[i].Parameters) == 0 {
			t.Errorf("%s 缺少参数 schema", descs[i].Name)
		}
		var parsed map[string]any
		if err := json.Unmarshal(descs[i].Parameters, &parsed); err != nil {
			t.Errorf("%s 的 schema 不是合法 JSON 对象: %v", descs[i].Name, err)
			continue
		}
		if parsed["type"] != "object" {
			t.Errorf("%s 的 schema type = %v，期望 object", descs[i].Name, parsed["type"])
		}
	}
}

// 内置工具的 schema 必须声明出工具实际解析的那个参数名 ——
// schema 里写 expr、Call 里读 formula，模型就会一直传错。
func TestBuiltinSchemasDeclareExpectedProperties(t *testing.T) {
	reg := NewRegistry(Calculator{}, NewHTTPTool(), TimeTool{}, CodeRunner{})
	want := map[string][]string{
		"calculator":   {"expr"},
		"http_request": {"url", "method"},
		"code_runner":  {"expr"},
		"time":         {},
	}
	for _, d := range reg.Descriptors() {
		var parsed struct {
			Properties map[string]any `json:"properties"`
			Required   []string       `json:"required"`
		}
		if err := json.Unmarshal(d.Parameters, &parsed); err != nil {
			t.Fatalf("%s schema 解析失败: %v", d.Name, err)
		}
		keys, ok := want[d.Name]
		if !ok {
			t.Fatalf("未预期的工具 %q", d.Name)
		}
		if len(parsed.Properties) != len(keys) {
			t.Errorf("%s 声明了 %d 个参数，期望 %d 个：%v",
				d.Name, len(parsed.Properties), len(keys), parsed.Properties)
		}
		for _, k := range keys {
			if _, ok := parsed.Properties[k]; !ok {
				t.Errorf("%s 的 schema 缺少参数 %q", d.Name, k)
			}
		}
		// 除 time 外都应声明 required：不声明的话模型容易传空对象过来
		if d.Name != "time" && len(parsed.Required) == 0 {
			t.Errorf("%s 未声明 required", d.Name)
		}
	}
}

func TestRegistryDescriptorsDefaultParametersForPlainTool(t *testing.T) {
	reg := NewRegistry(&plainTool{name: "bare"})
	descs := reg.Descriptors()
	if len(descs) != 1 {
		t.Fatalf("工具数 = %d", len(descs))
	}
	if len(descs[0].Parameters) == 0 {
		t.Fatal("未声明 schema 的工具必须拿到兜底 schema，否则无法放进 tools 数组")
	}
	var parsed map[string]any
	if err := json.Unmarshal(descs[0].Parameters, &parsed); err != nil {
		t.Fatalf("兜底 schema 不是合法 JSON: %v", err)
	}
	// 必须是"接受任意对象"而不是 null：null 会被部分厂商拒绝
	if parsed["type"] != "object" {
		t.Errorf("兜底 schema type = %v，期望 object", parsed["type"])
	}
}

func TestRegistryDescriptorsForFiltersAndReportsUnknown(t *testing.T) {
	reg := NewRegistry(Calculator{}, TimeTool{}, CodeRunner{})

	descs, unknown := reg.DescriptorsFor([]string{"time", "calculator"})
	if len(descs) != 2 {
		t.Fatalf("过滤后工具数 = %d，期望 2", len(descs))
	}
	// 显式清单的顺序由调用方给定（用户写的是什么顺序就是什么顺序）
	if descs[0].Name != "time" || descs[1].Name != "calculator" {
		t.Errorf("过滤结果顺序 = %v", []string{descs[0].Name, descs[1].Name})
	}
	if len(unknown) != 0 {
		t.Errorf("不应有未注册项，实际 %v", unknown)
	}

	// 未注册的名字必须被报出来，而不是静默丢掉：
	// "配置里工具名写错"和"模型决定不调工具"是两回事，前者需要人看见。
	_, unknown = reg.DescriptorsFor([]string{"calculator", "does_not_exist"})
	if len(unknown) != 1 || unknown[0] != "does_not_exist" {
		t.Errorf("未注册项 = %v，期望 [does_not_exist]", unknown)
	}

	// 空清单 = 全部
	descs, unknown = reg.DescriptorsFor(nil)
	if len(descs) != 3 || len(unknown) != 0 {
		t.Errorf("空清单应返回全部 3 个，实际 %d 个，unknown=%v", len(descs), unknown)
	}
}

func TestRegistryDescriptorsForDeduplicates(t *testing.T) {
	reg := NewRegistry(Calculator{})
	descs, unknown := reg.DescriptorsFor([]string{"calculator", "calculator"})
	if len(descs) != 1 {
		t.Errorf("重复的工具名应去重，实际返回 %d 个", len(descs))
	}
	if len(unknown) != 0 {
		t.Errorf("unknown = %v", unknown)
	}
}

// ---------- CallWithArguments 的兜底规则 ----------

func TestCallWithArgumentsUnwrapsSingleFieldForPlainTool(t *testing.T) {
	pt := &plainTool{name: "bare"}
	reg := NewRegistry(pt)

	out, err := reg.CallWithArguments(context.Background(), "bare", `{"anything":"hello"}`)
	if err != nil {
		t.Fatalf("调用失败: %v", err)
	}
	if len(pt.got) != 1 || pt.got[0] != "hello" {
		t.Errorf("单字段 JSON 对象应被解包成其值，实际收到 %q", pt.got)
	}
	if out != "got:hello" {
		t.Errorf("返回值 = %q", out)
	}
}

func TestCallWithArgumentsPassesThroughMultiFieldJSONForPlainTool(t *testing.T) {
	pt := &plainTool{name: "bare"}
	reg := NewRegistry(pt)

	raw := `{"a":"1","b":"2"}`
	if _, err := reg.CallWithArguments(context.Background(), "bare", raw); err != nil {
		t.Fatalf("调用失败: %v", err)
	}
	// 多字段时原样透传：错误地拆包会让工具收到一个语法正确但语义全错的入参，
	// 而原样透传至少能让工具自己报错。
	if len(pt.got) != 1 || pt.got[0] != raw {
		t.Errorf("多字段 JSON 应原样透传，实际收到 %q", pt.got)
	}
}

func TestCallWithArgumentsPassesThroughPlainText(t *testing.T) {
	pt := &plainTool{name: "bare"}
	reg := NewRegistry(pt)

	want := []string{"1+1", "{}", `"quoted"`}
	for _, args := range want {
		if _, err := reg.CallWithArguments(context.Background(), "bare", args); err != nil {
			t.Fatalf("%s: 调用失败: %v", args, err)
		}
	}
	if len(pt.got) != len(want) {
		t.Fatalf("收到 %d 次调用，期望 %d 次", len(pt.got), len(want))
	}
	for i, w := range want {
		if pt.got[i] != w {
			t.Errorf("第 %d 次入参 = %q，期望 %q", i, pt.got[i], w)
		}
	}
}

func TestCallWithArgumentsUnknownTool(t *testing.T) {
	reg := NewRegistry(Calculator{})
	_, err := reg.CallWithArguments(context.Background(), "nope", `{}`)
	if err == nil {
		t.Fatal("未注册的工具应报错")
	}
	if !errors.Is(err, ErrUnknownTool) {
		t.Errorf("错误应可用 errors.Is 匹配 ErrUnknownTool，实际 %v", err)
	}
}

// ---------- 内置工具的参数翻译 ----------

func TestBuiltinToolsTranslateArguments(t *testing.T) {
	cases := []struct {
		name  string
		tool  Tool
		args  string
		want  string
		isErr bool
	}{
		// 标准写法：按 schema 给 {"expr": "..."}
		// 注意 (12+34)*5/2 = 46*5/2 = 115
		{"calculator 标准入参", Calculator{}, `{"expr":"(12+34)*5/2"}`, "115", false},
		{"calculator 别名 expression", Calculator{}, `{"expression":"2*3"}`, "6", false},
		{"calculator 唯一字段换个名字", Calculator{}, `{"formula":"7-2"}`, "5", false},
		{"calculator 裸 JSON 字符串", Calculator{}, `"8/2"`, "4", false},
		{"calculator 完全裸的文本", Calculator{}, `9-1`, "8", false},

		{"code_runner 标准入参", CodeRunner{}, `{"expr":"sum([1,2,3])"}`, "6", false},
		{"code_runner 别名 code", CodeRunner{}, `{"code":"avg([10,20])"}`, "15", false},
		{"code_runner 裸文本", CodeRunner{}, `max([3,9,4])`, "9", false},

		// 多字段时**只有按键取值**才拿得到参数。
		// 没有下面这两条用例的话，"argFromJSON 的 keys 参数"其实完全没有被覆盖：
		// 上面所有输入都是单字段对象，走的是"唯一字段兜底"那条分支，
		// 把 keys 改成任意值测试依然全绿。
		{"calculator 多字段按键取值", Calculator{}, `{"expr":"2+2","note":"随便写的"}`, "4", false},
		{"code_runner 多字段按键取值", CodeRunner{}, `{"expr":"sum([1,2,3])","lang":"py"}`, "6", false},
		{"calculator 多字段但键名都不认识", Calculator{}, `{"a":"1","b":"2"}`, "", true},

		// 模型不按 schema 出牌时的安全性：白名单仍然拦得住
		{"calculator 拒绝代码注入", Calculator{}, `{"expr":"__import__('os')"}`, "", true},
		{"calculator 拒绝字母", Calculator{}, `{"expr":"open('x')"}`, "", true},
		{"code_runner 拒绝任意函数", CodeRunner{}, `{"expr":"os.system('ls')"}`, "", true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ac, ok := c.tool.(ArgumentCaller)
			if !ok {
				t.Fatalf("%T 必须实现 ArgumentCaller", c.tool)
			}
			out, err := ac.CallWithArguments(context.Background(), c.args)
			if c.isErr {
				if err == nil {
					t.Fatalf("应报错，实际返回 %q", out)
				}
				return
			}
			if err != nil {
				t.Fatalf("不应报错: %v", err)
			}
			if out != c.want {
				t.Errorf("结果 = %q，期望 %q", out, c.want)
			}
		})
	}
}

// time 工具忽略任何入参，但必须能被 function calling 调用（返回合法时间）。
func TestTimeToolCallWithArguments(t *testing.T) {
	for _, args := range []string{`{}`, "", `{"ignored":"x"}`} {
		out, err := TimeTool{}.CallWithArguments(context.Background(), args)
		if err != nil {
			t.Fatalf("入参 %q 失败: %v", args, err)
		}
		if _, err := time.Parse(time.RFC3339, out); err != nil {
			t.Errorf("入参 %q 的返回值 %q 不是 RFC3339 时间", args, out)
		}
	}
}

// http_request 需要完整的 JSON 对象（两个字段），所以它的 CallWithArguments
// 是把参数原样交给 Call —— 兜底的解包规则不能把 url 单独拆出来。
func TestHTTPToolCallWithArgumentsKeepsURLAndMethod(t *testing.T) {
	var gotMethod string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		_, _ = io.WriteString(w, "pong")
	}))
	defer srv.Close()

	args, _ := json.Marshal(map[string]string{"url": srv.URL, "method": "POST"})
	out, err := NewHTTPTool().CallWithArguments(context.Background(), string(args))
	if err != nil {
		t.Fatalf("失败: %v", err)
	}
	if strings.TrimSpace(out) != "pong" {
		t.Errorf("返回 = %q", out)
	}
	if gotMethod != "POST" {
		t.Errorf("method = %q，期望 POST（多字段参数被错误解包了？）", gotMethod)
	}
}

// 所有内置工具都必须实现这两个可选接口，
// 否则会静默退化到兜底规则，而兜底规则对多参工具（http_request）是错的。
func TestAllBuiltinToolsImplementOptionalInterfaces(t *testing.T) {
	tools := []Tool{Calculator{}, NewHTTPTool(), TimeTool{}, CodeRunner{}}
	for _, tl := range tools {
		if _, ok := tl.(SchemaProvider); !ok {
			t.Errorf("%T 未实现 SchemaProvider，模型看不到参数说明", tl)
		}
		if _, ok := tl.(ArgumentCaller); !ok {
			t.Errorf("%T 未实现 ArgumentCaller，会退化到兜底解包规则", tl)
		}
	}
}
