import { Monitor, Moon, Sun } from "lucide-react";
import { useTheme, type ThemeMode } from "@/lib/theme";
import { cn } from "@/lib/utils";

const OPTIONS: { mode: ThemeMode; icon: typeof Sun; label: string }[] = [
  { mode: "light", icon: Sun, label: "浅色" },
  { mode: "dark", icon: Moon, label: "深色" },
  { mode: "system", icon: Monitor, label: "跟随系统" },
];

/** 三态主题切换：浅色 / 深色 / 跟随系统。 */
export function ThemeToggle() {
  const mode = useTheme((s) => s.mode);
  const setMode = useTheme((s) => s.setMode);

  return (
    <div className="inline-flex items-center gap-0.5 rounded-lg border border-line bg-surface-2 p-0.5">
      {OPTIONS.map(({ mode: m, icon: Icon, label }) => {
        const active = mode === m;
        return (
          <button
            key={m}
            onClick={() => setMode(m)}
            title={label}
            aria-label={label}
            aria-pressed={active}
            className={cn(
              "flex h-[26px] w-[26px] items-center justify-center rounded-md",
              "transition-all duration-150 ease-smooth",
              active
                ? "bg-surface text-fg shadow-xs"
                : "text-fg-subtle hover:text-fg",
            )}
          >
            <Icon size={13.5} strokeWidth={2} />
          </button>
        );
      })}
    </div>
  );
}
