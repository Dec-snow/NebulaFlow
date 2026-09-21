import { cn, hueOf } from "@/lib/utils";

/** 头像：由用户名派生出稳定的渐变，无需上传图片也有辨识度。 */
export function Avatar({
  name,
  size = 32,
  className,
}: {
  name: string;
  size?: number;
  className?: string;
}) {
  const h = hueOf(name || "?");
  const initial = (name?.[0] ?? "?").toUpperCase();

  return (
    <div
      className={cn(
        "flex shrink-0 select-none items-center justify-center rounded-full font-semibold text-white",
        className,
      )}
      style={{
        width: size,
        height: size,
        fontSize: size * 0.4,
        background: `linear-gradient(135deg, hsl(${h} 72% 58%), hsl(${(h + 42) % 360} 76% 50%))`,
        boxShadow: `0 0 0 1px hsl(${h} 60% 50% / 0.25)`,
      }}
      title={name}
    >
      {initial}
    </div>
  );
}
