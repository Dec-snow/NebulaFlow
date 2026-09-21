import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

// 开发环境代理到 Go 后端，避免 CORS 配置
export default defineConfig({
  plugins: [react()],
  server: {
    port: 5173,
    proxy: {
      "/api": {
        target: process.env.VITE_API_TARGET || "http://localhost:8080",
        changeOrigin: true,
      },
    },
  },
  build: {
    outDir: "dist",
    sourcemap: false,
  },
});
