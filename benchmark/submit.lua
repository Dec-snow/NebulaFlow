-- wrk 压测脚本：先登录取 token，再并发提交任务
-- 用法：
--   TOKEN=$(curl -s -X POST localhost:8080/api/auth/login -d '{"username":"demo","password":"demo123456"}' | jq -r .token)
--   wrk -t8 -c50 -d10s -s benchmark/submit.lua http://localhost:8080/api/tasks -- -T "$TOKEN"

local token = arg[1] or os.getenv("TOKEN")
local body = '{"workflow_id":1,"input":"压测输入：' .. tostring(os.time()) .. '"}'

wrk.method = "POST"
wrk.headers["Content-Type"] = "application/json"
if token and token ~= "" then
  wrk.headers["Authorization"] = "Bearer " .. token
end
wrk.body = body
