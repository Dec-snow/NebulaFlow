/** @type {import('tailwindcss').Config} */
export default {
  content: ["./index.html", "./src/**/*.{ts,tsx}"],
  theme: {
    extend: {
      colors: {
        // NebulaFlow 深空主题
        nebula: {
          950: "#070b14",
          900: "#0b1120",
          800: "#111a30",
          700: "#1a2440",
          500: "#3b5bbf",
          400: "#5b7cff",
          300: "#8aa3ff",
        },
      },
      fontFamily: {
        mono: ["JetBrains Mono", "ui-monospace", "SFMono-Regular", "Menlo", "monospace"],
      },
    },
  },
  plugins: [],
};
