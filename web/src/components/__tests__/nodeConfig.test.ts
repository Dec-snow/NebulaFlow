import { describe, it, expect } from "vitest";
import {
  computeUpstreamNodes,
  topologicalSort,
  formatDuration,
  validateNodeName,
} from "../../utils/nodeUtils";
import type { NodeType } from "../../types";

describe("computeUpstreamNodes", () => {
  const edges = [
    { source_node: "input", target_node: "llm" },
    { source_node: "llm", target_node: "output" },
    { source_node: "rag", target_node: "llm" },
  ];

  it("计算直接上游节点", () => {
    const result = computeUpstreamNodes(edges, "llm");
    expect(result).toContain("input");
    expect(result).toContain("rag");
    expect(result.length).toBe(2);
  });

  it("计算所有上游节点（多级依赖）", () => {
    const result = computeUpstreamNodes(edges, "output");
    expect(result).toContain("llm");
    expect(result).toContain("input");
    expect(result).toContain("rag");
    expect(result.length).toBe(3);
  });

  it("没有上游节点时返回空数组", () => {
    const result = computeUpstreamNodes(edges, "input");
    expect(result).toEqual([]);
  });

  it("目标节点不存在时返回空数组", () => {
    const result = computeUpstreamNodes(edges, "nonexistent");
    expect(result).toEqual([]);
  });

  it("空边列表返回空数组", () => {
    const result = computeUpstreamNodes([], "output");
    expect(result).toEqual([]);
  });

  it("复杂 DAG 中正确计算所有上游", () => {
    const complexEdges = [
      { source_node: "a", target_node: "b" },
      { source_node: "a", target_node: "c" },
      { source_node: "b", target_node: "d" },
      { source_node: "c", target_node: "d" },
      { source_node: "d", target_node: "e" },
    ];
    const result = computeUpstreamNodes(complexEdges, "e");
    expect(result.sort()).toEqual(["a", "b", "c", "d"].sort());
  });
});

describe("topologicalSort", () => {
  const nodes = (keys: string[], type: NodeType = "llm") =>
    keys.map((k) => ({ node_key: k, node_type: type }));

  it("简单线性拓扑排序", () => {
    const ns = nodes(["input", "llm", "output"]);
    const edges = [
      { source_node: "input", target_node: "llm" },
      { source_node: "llm", target_node: "output" },
    ];
    const result = topologicalSort(ns, edges);
    // input 应该在 llm 前面，llm 应该在 output 前面
    expect(result.indexOf("input")).toBeLessThan(result.indexOf("llm"));
    expect(result.indexOf("llm")).toBeLessThan(result.indexOf("output"));
    expect(result.length).toBe(3);
  });

  it("多个入度为 0 的节点", () => {
    const ns = nodes(["a", "b", "c"]);
    const edges = [
      { source_node: "a", target_node: "c" },
      { source_node: "b", target_node: "c" },
    ];
    const result = topologicalSort(ns, edges);
    expect(result.indexOf("a")).toBeLessThan(result.indexOf("c"));
    expect(result.indexOf("b")).toBeLessThan(result.indexOf("c"));
    expect(result.length).toBe(3);
  });

  it("空节点列表返回空数组", () => {
    const result = topologicalSort([], []);
    expect(result).toEqual([]);
  });

  it("无边时按节点原始顺序返回", () => {
    const ns = nodes(["x", "y", "z"]);
    const result = topologicalSort(ns, []);
    expect(result.length).toBe(3);
    expect(result).toContain("x");
    expect(result).toContain("y");
    expect(result).toContain("z");
  });

  it("复杂 DAG 拓扑排序", () => {
    const ns = nodes(["a", "b", "c", "d", "e"]);
    const edges = [
      { source_node: "a", target_node: "b" },
      { source_node: "a", target_node: "c" },
      { source_node: "b", target_node: "d" },
      { source_node: "c", target_node: "d" },
      { source_node: "d", target_node: "e" },
    ];
    const result = topologicalSort(ns, edges);
    expect(result.indexOf("a")).toBeLessThan(result.indexOf("b"));
    expect(result.indexOf("a")).toBeLessThan(result.indexOf("c"));
    expect(result.indexOf("b")).toBeLessThan(result.indexOf("d"));
    expect(result.indexOf("c")).toBeLessThan(result.indexOf("d"));
    expect(result.indexOf("d")).toBeLessThan(result.indexOf("e"));
  });
});

describe("formatDuration", () => {
  it("毫秒级显示", () => {
    expect(formatDuration(0)).toBe("0ms");
    expect(formatDuration(500)).toBe("500ms");
    expect(formatDuration(999)).toBe("999ms");
  });

  it("秒级显示（保留两位小数）", () => {
    expect(formatDuration(1000)).toBe("1.00s");
    expect(formatDuration(1500)).toBe("1.50s");
    expect(formatDuration(2345)).toBe("2.35s");
    expect(formatDuration(59999)).toBe("60.00s");
  });

  it("分钟级显示", () => {
    expect(formatDuration(60000)).toBe("1m 0s");
    expect(formatDuration(90000)).toBe("1m 30s");
    expect(formatDuration(125000)).toBe("2m 5s");
    expect(formatDuration(3600000)).toBe("60m 0s");
  });

  it("边界值正确处理", () => {
    expect(formatDuration(999)).toBe("999ms");
    expect(formatDuration(1000)).toBe("1.00s");
    expect(formatDuration(59999)).toBe("60.00s");
    expect(formatDuration(60000)).toBe("1m 0s");
  });
});

describe("validateNodeName", () => {
  it("有效名称验证通过", () => {
    const result = validateNodeName("my_node-123", []);
    expect(result.valid).toBe(true);
    expect(result.error).toBeUndefined();
  });

  it("空名称验证失败", () => {
    expect(validateNodeName("", []).valid).toBe(false);
    expect(validateNodeName("", []).error).toBe("节点名称不能为空");
  });

  it("纯空格名称验证失败", () => {
    expect(validateNodeName("   ", []).valid).toBe(false);
    expect(validateNodeName("   ", []).error).toBe("节点名称不能为空");
  });

  it("名称过长验证失败", () => {
    const longName = "a".repeat(51);
    const result = validateNodeName(longName, []);
    expect(result.valid).toBe(false);
    expect(result.error).toBe("节点名称不能超过 50 个字符");
  });

  it("名称长度等于 50 时验证通过", () => {
    const name = "a".repeat(50);
    const result = validateNodeName(name, []);
    expect(result.valid).toBe(true);
  });

  it("包含非法字符验证失败", () => {
    expect(validateNodeName("my node", []).valid).toBe(false);
    expect(validateNodeName("my@node", []).valid).toBe(false);
    expect(validateNodeName("my#node", []).valid).toBe(false);
    expect(validateNodeName("节点名", []).valid).toBe(false);
  });

  it("重名验证失败", () => {
    const existingNames = ["input", "llm", "output"];
    const result = validateNodeName("llm", existingNames);
    expect(result.valid).toBe(false);
    expect(result.error).toBe('节点名称 "llm" 已存在');
  });

  it("不重名时验证通过", () => {
    const existingNames = ["input", "llm"];
    const result = validateNodeName("output", existingNames);
    expect(result.valid).toBe(true);
  });

  it("名称前后空格会被 trim 后验证", () => {
    const result = validateNodeName("  valid_name  ", []);
    expect(result.valid).toBe(true);
  });

  it("trim 后重名也验证失败", () => {
    const existingNames = ["llm"];
    const result = validateNodeName("  llm  ", existingNames);
    expect(result.valid).toBe(false);
  });
});
