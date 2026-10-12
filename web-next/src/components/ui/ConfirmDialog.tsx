import { useEffect, type ReactNode } from "react";
import { AlertTriangle, X } from "lucide-react";
import { Button } from "./Button";
import { cn } from "@/lib/utils";

export interface ConfirmDialogProps {
  /** 是否显示 */
  open: boolean;
  /** 标题 */
  title: string;
  /** 描述内容 */
  description?: ReactNode;
  /** 确认按钮文字 */
  confirmText?: string;
  /** 取消按钮文字 */
  cancelText?: string;
  /** 确认按钮色调 */
  tone?: "danger" | "brand";
  /** 确认回调 */
  onConfirm: () => void;
  /** 取消回调 */
  onCancel: () => void;
}

/**
 * 通用确认对话框：用于删除等危险操作的二次确认。
 */
export function ConfirmDialog({
  open,
  title,
  description,
  confirmText = "确认",
  cancelText = "取消",
  tone = "danger",
  onConfirm,
  onCancel,
}: ConfirmDialogProps) {
  // ESC 关闭
  useEffect(() => {
    if (!open) return;
    const handleKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") onCancel();
    };
    window.addEventListener("keydown", handleKey);
    return () => window.removeEventListener("keydown", handleKey);
  }, [open, onCancel]);

  if (!open) return null;

  return (
    <div
      className="fixed inset-0 z-50 flex items-center justify-center"
      role="dialog"
      aria-modal="true"
    >
      {/* 遮罩 */}
      <div
        className="absolute inset-0 bg-fg/30 backdrop-blur-sm animate-fade-in"
        onClick={onCancel}
      />
      {/* 对话框 */}
      <div
        className={cn(
          "relative z-10 w-full max-w-sm rounded-xl border border-line bg-surface p-5 shadow-xl animate-scale-in",
        )}
      >
        <button
          className="absolute right-3 top-3 rounded-md p-1 text-fg-subtle transition-colors hover:bg-surface-2 hover:text-fg"
          onClick={onCancel}
          aria-label="关闭"
        >
          <X size={14} />
        </button>

        <div className="flex items-start gap-3">
          <div
            className={cn(
              "flex h-10 w-10 shrink-0 items-center justify-center rounded-xl",
              tone === "danger" ? "bg-rose/12 text-rose" : "bg-brand-soft text-brand",
            )}
          >
            <AlertTriangle size={18} />
          </div>
          <div className="min-w-0 flex-1 pt-0.5">
            <h3 className="text-sm font-semibold text-fg">{title}</h3>
            {description && (
              <p className="mt-1.5 text-xs leading-relaxed text-fg-subtle">
                {description}
              </p>
            )}
          </div>
        </div>

        <div className="mt-5 flex justify-end gap-2">
          <Button variant="outline" size="sm" onClick={onCancel}>
            {cancelText}
          </Button>
          <Button
            variant={tone === "danger" ? "danger" : "brand"}
            size="sm"
            onClick={onConfirm}
          >
            {confirmText}
          </Button>
        </div>
      </div>
    </div>
  );
}
