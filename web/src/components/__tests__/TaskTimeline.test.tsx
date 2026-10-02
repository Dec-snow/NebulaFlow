import { describe, it, expect, vi } from "vitest";
import { render, screen, fireEvent } from "@testing-library/react";
import TaskTimeline from "../TaskTimeline";
import type { TaskNode, NodeType } from "../../types";

// Mock lucide-react icons to avoid SVG rendering issues
vi.mock("lucide-react", () => ({
  CheckCircle2: () => <span data-testid="icon-checkcircle2" />,
  Circle: () => <span data-testid="icon-circle" />,
  Loader2: () => <span data-testid="icon-loader2" />,
  XCircle: () => <span data-testid="icon-xcircle" />,
  Ban: () => <span data-testid="icon-ban" />,
  Clock: () => <span data-testid="icon-clock" />,
  ChevronDown: () => <span data-testid="icon-chevrondown" />,
  ChevronRight: () => <span data-testid="icon-chevronright" />,
  Copy: () => <span data-testid="icon-copy" />,
  Check: () => <span data-testid="icon-check" />,
  Zap: () => <span data-testid="icon-zap" />,
  AlertCircle: () => <span data-testid="icon-alertcircle" />,
  Hash: () => <span data-testid="icon-hash" />,
}));

// Mock FlowNodes nodeMeta
vi.mock("../FlowNodes", () => ({
  nodeMeta: {
    input: { label: "Input", color: "#8aa3ff", description: "任务输入入口" },
    llm: { label: "LLM", color: "#5b7cff", description: "大语言模型调用" },
    output: { label: "Output", color: "#f472b6", description: "结果输出节点" },
    rag: { label: "RAG", color: "#34d399", description: "知识库检索增强" },
    tool: { label: "Tool", color: "#fbbf24", description: "工具函数调用" },
  },
}));

// Mock navigator.clipboard
Object.defineProperty(navigator, "clipboard", {
  value: { writeText: vi.fn() },
  writable: true,
});

describe("TaskTimeline", () => {
  const workflowNodes: { node_key: string; node_type: NodeType }[] = [
    { node_key: "input", node_type: "input" },
    { node_key: "llm", node_type: "llm" },
    { node_key: "output", node_type: "output" },
  ];

  const workflowEdges: { source_node: string; target_node: string }[] = [
    { source_node: "input", target_node: "llm" },
    { source_node: "llm", target_node: "output" },
  ];

  const mockNodes: TaskNode[] = [
    {
      id: 1,
      task_id: 1,
      node_id: 1,
      node_key: "input",
      node_type: "input",
      status: "succeeded",
      input: "用户输入内容",
      output: "处理后的输入",
      retries: 0,
      tokens_in: 10,
      tokens_out: 10,
      duration_ms: 100,
    },
    {
      id: 2,
      task_id: 1,
      node_id: 2,
      node_key: "llm",
      node_type: "llm",
      status: "succeeded",
      input: "LLM 输入",
      output: "LLM 输出结果",
      retries: 1,
      tokens_in: 500,
      tokens_out: 200,
      duration_ms: 1500,
    },
    {
      id: 3,
      task_id: 1,
      node_id: 3,
      node_key: "output",
      node_type: "output",
      status: "pending",
      retries: 0,
      tokens_in: 0,
      tokens_out: 0,
      duration_ms: 0,
    },
  ];

  const defaultProps = {
    nodes: mockNodes,
    workflowNodes,
    workflowEdges,
    totalDuration: 1600,
    taskStartedAt: "2024-01-01T00:00:00Z",
    taskFinishedAt: "2024-01-01T00:00:01.6Z",
  };

  // 辅助函数：找到节点的可点击行
  const getNodeClickableRow = (nodeKey: string): HTMLElement => {
    const nodeEl = screen.getByText(nodeKey).closest(".group");
    expect(nodeEl).toBeTruthy();
    const clickable = nodeEl!.querySelector(".cursor-pointer");
    expect(clickable).toBeTruthy();
    return clickable as HTMLElement;
  };

  it("基本渲染：节点按拓扑顺序展示", () => {
    render(<TaskTimeline {...defaultProps} />);

    // 三个节点都应该渲染（通过 title 属性精确定位节点名称）
    expect(screen.getByTitle("input")).toBeInTheDocument();
    expect(screen.getByTitle("llm")).toBeInTheDocument();
    expect(screen.getByTitle("output")).toBeInTheDocument();

    // 验证拓扑顺序：input → llm → output
    const nodeTitles = screen
      .getAllByTitle(/^(input|llm|output)$/)
      .map((el) => el.getAttribute("title"));
    expect(nodeTitles).toEqual(["input", "llm", "output"]);
  });

  it("进度条正确显示：succeeded 节点有进度条，pending 节点只有圆点", () => {
    render(<TaskTimeline {...defaultProps} />);

    // 获取所有进度条轨道
    const tracks = document.querySelectorAll(".flex-1.relative.h-6");
    expect(tracks.length).toBe(3);

    // succeeded 状态的节点有进度条（bg-emerald-400 的 bar）
    const succeededBars = document.querySelectorAll(
      ".bg-emerald-400.rounded-full"
    );
    // input 和 llm 都是 succeeded，应该有 2 个进度条
    expect(succeededBars.length).toBeGreaterThanOrEqual(2);

    // pending 状态的节点只有圆点（bg-slate-700 + border-slate-600）
    const pendingDots = document.querySelectorAll(
      ".bg-slate-700.border.border-slate-600"
    );
    expect(pendingDots.length).toBe(1);
  });

  it("点击节点展开/收起详情", () => {
    render(<TaskTimeline {...defaultProps} />);

    // 初始状态：详情应该不显示
    expect(screen.queryByText("输入")).not.toBeInTheDocument();

    // 找到 llm 节点的可点击行并点击
    const llmClickable = getNodeClickableRow("llm");
    fireEvent.click(llmClickable);

    // 展开后应该显示详情内容
    expect(screen.getByText("输入")).toBeInTheDocument();
    expect(screen.getByText("输出")).toBeInTheDocument();
    expect(screen.getByText("LLM 输入")).toBeInTheDocument();
    expect(screen.getByText("LLM 输出结果")).toBeInTheDocument();

    // 再次点击收起
    fireEvent.click(llmClickable);
    expect(screen.queryByText("输入")).not.toBeInTheDocument();
  });

  it("显示正确的状态颜色", () => {
    render(<TaskTimeline {...defaultProps} />);

    // succeeded 状态的节点应该有 emerald 颜色文字
    const inputNodeRow = screen.getByTitle("input").closest(".group");
    expect(inputNodeRow?.querySelector(".text-emerald-400")).toBeTruthy();

    // pending 状态的节点应该有 slate-500 颜色文字
    const outputNodeRow = screen.getByTitle("output").closest(".group");
    expect(outputNodeRow?.querySelector(".text-slate-500")).toBeTruthy();
  });

  it("显示 token 统计", () => {
    render(<TaskTimeline {...defaultProps} />);

    // 展开 llm 节点
    const llmClickable = getNodeClickableRow("llm");
    fireEvent.click(llmClickable);

    // 展开详情中应该显示 token 信息（通过 hash 图标找到父级容器）
    const tokenRow = screen.getByTestId("icon-hash").parentElement;
    expect(tokenRow?.textContent).toContain("500");
    expect(tokenRow?.textContent).toContain("200");
    expect(tokenRow?.textContent).toContain("in /");
    expect(tokenRow?.textContent).toContain("out");
  });

  it("显示依赖关系", () => {
    render(<TaskTimeline {...defaultProps} />);

    // 点击 llm 节点展开详情
    const llmClickable = getNodeClickableRow("llm");
    fireEvent.click(llmClickable);

    // 应该显示上游依赖
    expect(screen.getByText("上游依赖")).toBeInTheDocument();
    // llm 的上游是 input
    const upstreamBadges = screen
      .getByText("上游依赖")
      .parentElement?.parentElement?.querySelectorAll("span.font-mono");
    expect(upstreamBadges?.length).toBe(1);
    expect(upstreamBadges?.[0].textContent).toBe("input");
  });

  it("显示总耗时和节点数统计", () => {
    render(<TaskTimeline {...defaultProps} />);

    expect(screen.getByText("总耗时：")).toBeInTheDocument();
    expect(screen.getByText("节点：")).toBeInTheDocument();
    // 节点数为 3（找到包含 "节点：" 的 span，其内部的第二个 span 是数字）
    const nodeCountContainer = screen.getByText("节点：").closest("span");
    expect(nodeCountContainer?.textContent).toContain("3");
  });

  it("显示重试次数", () => {
    render(<TaskTimeline {...defaultProps} />);

    // 展开 llm 节点
    const llmClickable = getNodeClickableRow("llm");
    fireEvent.click(llmClickable);

    // llm 节点有 1 次重试（通过 alertcircle 图标找到父级容器）
    const retryRow = screen.getByTestId("icon-alertcircle").parentElement;
    expect(retryRow?.textContent).toContain("重试");
    expect(retryRow?.textContent).toContain("1");
    expect(retryRow?.textContent).toContain("次");
  });
});
