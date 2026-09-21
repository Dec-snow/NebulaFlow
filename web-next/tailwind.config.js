/** @type {import('tailwindcss').Config} */
export default {
  darkMode: "class",
  content: ["./index.html", "./src/**/*.{ts,tsx}"],
  theme: {
    extend: {
      // 语义色全部走 CSS 变量，双主题切换时无需重编译
      colors: {
        bg: "rgb(var(--c-bg) / <alpha-value>)",
        surface: "rgb(var(--c-surface) / <alpha-value>)",
        "surface-2": "rgb(var(--c-surface-2) / <alpha-value>)",
        "surface-3": "rgb(var(--c-surface-3) / <alpha-value>)",
        line: "rgb(var(--c-line) / <alpha-value>)",
        "line-strong": "rgb(var(--c-line-strong) / <alpha-value>)",
        fg: "rgb(var(--c-fg) / <alpha-value>)",
        "fg-muted": "rgb(var(--c-fg-muted) / <alpha-value>)",
        "fg-subtle": "rgb(var(--c-fg-subtle) / <alpha-value>)",
        brand: "rgb(var(--c-brand) / <alpha-value>)",
        "brand-fg": "rgb(var(--c-brand-fg) / <alpha-value>)",
        "brand-soft": "rgb(var(--c-brand-soft) / <alpha-value>)",
        "brand-ring": "rgb(var(--c-brand-ring) / <alpha-value>)",
        mint: "rgb(var(--c-mint) / <alpha-value>)",
        sky: "rgb(var(--c-sky) / <alpha-value>)",
        amber: "rgb(var(--c-amber) / <alpha-value>)",
        rose: "rgb(var(--c-rose) / <alpha-value>)",
        violet: "rgb(var(--c-violet) / <alpha-value>)",
      },
      fontFamily: {
        sans: [
          "Inter",
          "-apple-system",
          "BlinkMacSystemFont",
          "Segoe UI",
          "PingFang SC",
          "Hiragino Sans GB",
          "Microsoft YaHei",
          "sans-serif",
        ],
        mono: ["JetBrains Mono", "ui-monospace", "SFMono-Regular", "Menlo", "monospace"],
      },
      fontSize: {
        "2xs": ["0.6875rem", { lineHeight: "1rem" }],
      },
      // 设计系统里用了一批细粒度的不透明度（12/14/15/35/45），
      // 它们不在 Tailwind 默认 opacity scale 内，若直接写 bg-mint/12
      // 会报 "class does not exist"。在这里补进 scale，
      // 好处是 bg-*/12 这类写法在 @apply 与 JSX 中都能用，无需退化成任意值语法。
      opacity: {
        12: "0.12",
        14: "0.14",
        15: "0.15",
        35: "0.35",
        45: "0.45",
      },
      borderRadius: {
        lg: "0.625rem",
        xl: "0.875rem",
        "2xl": "1.125rem",
        "3xl": "1.5rem",
      },
      boxShadow: {
        xs: "var(--sh-xs)",
        sm: "var(--sh-sm)",
        md: "var(--sh-md)",
        lg: "var(--sh-lg)",
        glow: "var(--sh-glow)",
      },
      transitionTimingFunction: {
        smooth: "cubic-bezier(0.32, 0.72, 0, 1)",
      },
      keyframes: {
        "fade-up": {
          from: { opacity: "0", transform: "translateY(6px)" },
          to: { opacity: "1", transform: "translateY(0)" },
        },
        "fade-in": {
          from: { opacity: "0" },
          to: { opacity: "1" },
        },
        shimmer: {
          "100%": { transform: "translateX(100%)" },
        },
        breathe: {
          "0%, 100%": { opacity: "1" },
          "50%": { opacity: "0.45" },
        },
        "draw-line": {
          from: { strokeDashoffset: "1" },
          to: { strokeDashoffset: "0" },
        },
      },
      animation: {
        "fade-up": "fade-up 0.4s cubic-bezier(0.32, 0.72, 0, 1) both",
        "fade-in": "fade-in 0.3s ease both",
        shimmer: "shimmer 1.6s infinite",
        breathe: "breathe 2s ease-in-out infinite",
      },
    },
  },
  plugins: [],
};
