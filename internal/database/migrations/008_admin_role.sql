-- 为 users 表添加 role 字段，用于权限隔离（admin / user）。
-- 默认值为 'user'，已有用户自动获得普通用户角色。

BEGIN;

ALTER TABLE users
    ADD COLUMN IF NOT EXISTS role VARCHAR(16) NOT NULL DEFAULT 'user';

COMMIT;
