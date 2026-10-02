import { Navigate, Route, Routes } from "react-router-dom";
import { useAuth } from "./store/auth";
import Layout from "./components/Layout";
import Login from "./pages/Login";
import Dashboard from "./pages/Dashboard";
import Workflows from "./pages/Workflows";
import WorkflowEditor from "./pages/WorkflowEditor";
import TaskDetail from "./pages/TaskDetail";
import Knowledge from "./pages/Knowledge";
import Models from "./pages/Models";
import Tasks from "./pages/Tasks";
import Agents from "./pages/Agents";
import AgentMarketplace from "./pages/AgentMarketplace";
import Templates from "./pages/Templates";

function RequireAuth({ children }: { children: JSX.Element }) {
  const token = useAuth((s) => s.token);
  if (!token) return <Navigate to="/login" replace />;
  return children;
}

export default function App() {
  return (
    <Routes>
      <Route path="/login" element={<Login />} />
      <Route
        path="/"
        element={
          <RequireAuth>
            <Layout />
          </RequireAuth>
        }
      >
        <Route index element={<Dashboard />} />
        <Route path="workflows" element={<Workflows />} />
        <Route path="workflows/:id" element={<WorkflowEditor />} />
        <Route path="tasks" element={<Tasks />} />
        <Route path="tasks/:id" element={<TaskDetail />} />
        <Route path="templates" element={<Templates />} />
        <Route path="agents" element={<Agents />} />
        <Route path="agents/marketplace" element={<AgentMarketplace />} />
        <Route path="knowledge" element={<Knowledge />} />
        <Route path="models" element={<Models />} />
      </Route>
    </Routes>
  );
}
