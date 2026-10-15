-- 010: 系统模板的归属改为 NULL（user_id 可空）
--
-- 缺陷：模板市场把"系统模板"写成 user_id = 0，但 workflows.user_id 是
--   BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE
-- 而 users.id 由 BIGSERIAL 生成、永远从 1 开始 —— 库里不存在 id = 0 的用户，
-- 于是每一条系统模板 INSERT 都被外键挡下（SQLSTATE 23503，
-- 约束名 workflows_user_id_fkey）。cmd/seed 当时只 log.Printf 不返回错误，
-- 所以 seed 退出码仍是 0，故障被完全掩盖：线上模板市场因此一直返回空数组。
-- （agents 表能用 user_id = 0 只是因为 agent_registry.user_id 没有外键，
--   003_agent_registry.sql 里它只是 NOT NULL DEFAULT 0。）
--
-- 修法：允许 NULL，用 NULL 表示"不属于任何用户"的系统模板。
--   1. 语义正确：系统模板确实没有归属用户，不是某个账号的私有物；
--   2. 外键天然满足：PostgreSQL 的 FK 不校验 NULL；
--   3. 归属校验只紧不松：`WHERE user_id = $n` 与 NULL 比较恒为 UNKNOWN，
--      所以普通用户永远无法把系统模板当成自己的 workflow 去读/改/删，
--      只能经 GetTemplate / CreateFromTemplate 这条只读+复制的路径使用它；
--   4. 不需要数据回填：0 从来没写进去过（一直被 FK 拒绝），
--      下面的 UPDATE 只是把"被手工绕过外键写入 0"这种异常状态归一成 NULL。
--
-- 注意：Go 侧对外仍然用 0 作为哨兵值（model.SystemTemplateUserID），
-- 由 store 层在读写边界做 0 <-> NULL 的翻译，因此内存实现与前端契约都不变。

BEGIN;

ALTER TABLE workflows
    ALTER COLUMN user_id DROP NOT NULL;

UPDATE workflows SET user_id = NULL
 WHERE is_template = true AND user_id = 0;

COMMIT;
