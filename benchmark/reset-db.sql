-- 压测前置：清空任务相关数据，保留用户 / 工作流 / 知识库 / Provider，
-- 保证每轮压测从同一个基线出发，任务 ID 也从 1 重新开始。
TRUNCATE tasks, task_nodes, task_logs, usage_records RESTART IDENTITY CASCADE;
