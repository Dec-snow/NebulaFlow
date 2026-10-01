-- 006_priority_retry.sql
-- 任务优先级 + 节点重试

-- 1. 任务优先级（high / normal / low）
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS priority VARCHAR(16) NOT NULL DEFAULT 'normal';

-- 索引：按优先级查询（调度时按优先级取）
CREATE INDEX IF NOT EXISTS idx_tasks_priority_status ON tasks (priority, status);

-- 2. 节点级重试信息
ALTER TABLE task_nodes ADD COLUMN IF NOT EXISTS retry_count INT NOT NULL DEFAULT 0;
ALTER TABLE task_nodes ADD COLUMN IF NOT EXISTS max_retry INT NOT NULL DEFAULT 0;
ALTER TABLE task_nodes ADD COLUMN IF NOT EXISTS retry_backoff VARCHAR(32) NOT NULL DEFAULT 'exponential';

-- 说明：
--   retry_count:  已重试次数（0 表示第一次执行，还没重试过）
--   max_retry:    最大重试次数（0 表示不重试，失败即结束）
--   retry_backoff: 退避策略：exponential（指数退避）/ fixed（固定间隔）/ immediate（立即重试）
