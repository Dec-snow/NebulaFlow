import React from "react";
import ReactDOM from "react-dom/client";
import { BrowserRouter } from "react-router-dom";
import App from "./App";
// 直接引用模块而不是 @/components/ui 桶文件，避免把整套 UI 库拉进入口 chunk
import { ErrorBoundary } from "@/components/ui/ErrorBoundary";
import "./index.css";

// AppShell 里已经有一层 ErrorBoundary（只兜住 <Outlet/> 页面内容），
// 登录页、侧边栏、顶栏以及路由初始化本身的异常没有兜底。
// 这里再包一层顶层边界，任何渲染期异常都降级成可重试的提示而不是白屏。
ReactDOM.createRoot(document.getElementById("root")!).render(
  <React.StrictMode>
    <ErrorBoundary
      title="控制台加载失败"
      description="应用初始化时发生错误。可以点重试，或刷新页面再试一次。"
    >
      <BrowserRouter>
        <App />
      </BrowserRouter>
    </ErrorBoundary>
  </React.StrictMode>,
);
