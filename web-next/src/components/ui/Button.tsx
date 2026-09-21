import type { ButtonHTMLAttributes, ReactNode } from "react";
import { cn } from "@/lib/utils";

type Variant = "brand" | "soft" | "outline" | "ghost" | "danger";

const VARIANT: Record<Variant, string> = {
  brand: "btn-brand",
  soft: "btn-soft",
  outline: "btn-outline",
  ghost: "btn-ghost",
  danger: "btn-danger",
};

export interface ButtonProps extends ButtonHTMLAttributes<HTMLButtonElement> {
  variant?: Variant;
  size?: "md" | "sm";
  /** 左侧图标 */
  icon?: ReactNode;
  /** 右侧内容（图标或快捷键提示） */
  trailing?: ReactNode;
  /** 撑满父容器宽度 */
  block?: boolean;
}

export function Button({
  variant = "outline",
  size = "md",
  icon,
  trailing,
  block,
  className,
  children,
  ...rest
}: ButtonProps) {
  return (
    <button
      className={cn(
        VARIANT[variant],
        size === "sm" && "btn-sm",
        block && "w-full",
        className,
      )}
      {...rest}
    >
      {icon}
      {children}
      {trailing}
    </button>
  );
}

export interface IconButtonProps extends ButtonHTMLAttributes<HTMLButtonElement> {
  variant?: Variant;
  /** 无障碍标签，同时作为 title 提示 */
  label: string;
}

export function IconButton({
  variant = "ghost",
  label,
  className,
  children,
  ...rest
}: IconButtonProps) {
  return (
    <button
      aria-label={label}
      title={label}
      className={cn(VARIANT[variant], "btn-icon", className)}
      {...rest}
    >
      {children}
    </button>
  );
}
