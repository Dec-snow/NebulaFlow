import { AlertTriangle, RefreshCw } from "lucide-react";
import { Button } from "./Button";

export interface ErrorStateProps {
  /** 错误标题 */
  title?: string;
  /** 错误描述 */
  description?: string;
  /** 重试回调 */
  onRetry?: () => void;
  /** 重试按钮文字 */
  retryText?: string;
  /** 自定义类名 */
  className?: string;
}

/**
 * 通用错误状态组件：数据加载失败时展示，带重试按钮。
 */
export function ErrorState({
  title = "加载失败",
  description = "请检查网络连接后重试。",
  onRetry,
  retryText = "重新加载",
  className,
}: ErrorStateProps) {
  return (
    <div
      className={`flex flex-col items-center justify-center rounded-xl border border-dashed border-line bg-surface/40 px-6 py-14 text-center ${className ?? ""}`}
    >
      <div className="mb-3.5 flex h-11 w-11 items-center justify-center rounded-xl bg-rose/12 text-rose">
        <AlertTriangle size={19} />
      </div>
      <h3 className="text-sm font-semibold text-fg">{title}</h3>
      <p className="mt-1.5 max-w-sm text-xs leading-relaxed text-fg-subtle">
        {description}
      </p>
      {onRetry && (
        <div className="mt-5">
          <Button variant="outline" size="sm" icon={<RefreshCw size={13} />} onClick={onRetry}>
            {retryText}
          </Button>
        </div>
      )}
    </div>
  );
}
