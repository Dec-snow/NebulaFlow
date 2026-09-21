import { cn } from "@/lib/utils";

export interface SegmentedOption<T extends string> {
  value: T;
  label: string;
  count?: number;
}

/** 分段控件：轨道 + 滑块，比一排按钮更克制。 */
export function Segmented<T extends string>({
  value,
  onChange,
  options,
  className,
}: {
  value: T;
  onChange: (v: T) => void;
  options: SegmentedOption<T>[];
  className?: string;
}) {
  return (
    <div
      className={cn(
        "inline-flex items-center gap-0.5 rounded-lg border border-line bg-surface-2 p-0.5",
        className,
      )}
      role="tablist"
    >
      {options.map((o) => {
        const active = o.value === value;
        return (
          <button
            key={o.value}
            role="tab"
            aria-selected={active}
            onClick={() => onChange(o.value)}
            className={cn(
              "inline-flex items-center gap-1.5 rounded-md px-2.5 py-1.5 text-xs font-medium",
              "transition-all duration-150 ease-smooth",
              active
                ? "bg-surface text-fg shadow-xs"
                : "text-fg-muted hover:text-fg",
            )}
          >
            {o.label}
            {o.count != null && (
              <span
                className={cn(
                  "tnum rounded px-1 text-2xs",
                  active ? "bg-brand-soft text-brand" : "bg-surface-3 text-fg-subtle",
                )}
              >
                {o.count}
              </span>
            )}
          </button>
        );
      })}
    </div>
  );
}
