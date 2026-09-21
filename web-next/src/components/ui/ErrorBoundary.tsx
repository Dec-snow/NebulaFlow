import { Component, type ErrorInfo, type ReactNode } from "react";
import { AlertTriangle, RefreshCw } from "lucide-react";
import { Button } from "./Button";

interface Props {
  children: ReactNode;
  /** 自定义降级标题 */
  title?: string;
  /** 自定义降级描述 */
  description?: string;
  /** 是否显示重试按钮 */
  retryable?: boolean;
  /** 重试回调 */
  onRetry?: () => void;
}

interface State {
  hasError: boolean;
  error: Error | null;
}

/**
 * Error Boundary：捕获子树渲染错误，展示降级界面。
 *
 * - 捕获渲染阶段错误（不捕获事件回调 / 异步错误）
 * - 支持自定义标题、描述、重试按钮
 * - 开发环境显示错误信息，生产环境隐藏堆栈
 */
export class ErrorBoundary extends Component<Props, State> {
  state: State = { hasError: false, error: null };

  static getDerivedStateFromError(error: Error): State {
    return { hasError: true, error };
  }

  componentDidCatch(error: Error, info: ErrorInfo) {
    // 上报到监控系统（预留）
    console.error("[ErrorBoundary]", error, info.componentStack);
  }

  handleRetry = () => {
    this.setState({ hasError: false, error: null });
    this.props.onRetry?.();
  };

  render() {
    if (!this.state.hasError) return this.props.children;

    const {
      title = "页面出现了问题",
      description = "组件渲染时发生了错误，请尝试刷新页面。",
      retryable = true,
    } = this.props;
    const { error } = this.state;
    const isDev = import.meta.env.DEV;

    return (
      <div className="flex min-h-[400px] w-full items-center justify-center p-6">
        <div className="flex max-w-md flex-col items-center text-center">
          <div className="mb-4 flex h-12 w-12 items-center justify-center rounded-full bg-rose/12 text-rose">
            <AlertTriangle size={24} />
          </div>
          <h3 className="text-base font-semibold text-fg">{title}</h3>
          <p className="mt-1.5 text-xs leading-relaxed text-fg-subtle">
            {description}
          </p>
          {isDev && error && (
            <div className="mt-4 w-full rounded-lg border border-rose/20 bg-rose/5 p-3 text-left">
              <div className="mb-1 text-xs font-medium text-rose">错误信息（开发环境）</div>
              <pre className="max-h-32 overflow-auto font-mono text-[11px] leading-relaxed text-rose/80">
                {error.message}
              </pre>
            </div>
          )}
          {retryable && (
            <Button
              variant="outline"
              size="sm"
              icon={<RefreshCw size={13} />}
              onClick={this.handleRetry}
              className="mt-5"
            >
              重试
            </Button>
          )}
        </div>
      </div>
    );
  }
}
