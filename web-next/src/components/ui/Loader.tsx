import { Loader2 } from "lucide-react";
import { cn } from "@/lib/utils";

interface Props {
  /** 提示文字 */
  text?: string;
  /** 尺寸：sm 紧凑 / md 默认 / lg 大 */
  size?: "sm" | "md" | "lg";
  /** 自定义类名 */
  className?: string;
}

/**
 * 页面级加载态：居中 spinner + 文字提示。
 * 用于数据量大、首次加载需要一点时间的场景。
 */
export function PageLoader({ text = "加载中…", size = "md", className }: Props) {
  const sizeMap = {
    sm: { icon: 14, text: "text-2xs" },
    md: { icon: 18, text: "text-xs" },
    lg: { icon: 24, text: "text-sm" },
  }[size];

  return (
    <div className={cn("flex w-full items-center justify-center py-16", className)}>
      <div className="flex flex-col items-center gap-3">
        <Loader2 size={sizeMap.icon} className="animate-spin text-brand" />
        <span className={cn("text-fg-subtle", sizeMap.text)}>{text}</span>
      </div>
    </div>
  );
}

/** 行内加载态：与文字同行的 spinner */
export function InlineLoader({ text = "加载中…" }: { text?: string }) {
  return (
    <span className="inline-flex items-center gap-2 text-xs text-fg-subtle">
      <Loader2 size={13} className="animate-spin text-brand" />
      {text}
    </span>
  );
}
