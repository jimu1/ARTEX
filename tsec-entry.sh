#!/bin/bash
# TSecBench 托管模式入口:容器启动后自行开始解题。
# 流程:内置 PostgreSQL -> ARTEX 服务 -> 预置 txzc 网关 profile -> 一题一任务编排器。
# 环境变量(平台注入):BENCHMARK_BASE_URL / BENCHMARK_TOKEN / TXZC_DEEPSEEK_API_KEY
set -euo pipefail
export PGDATA=/tmp/pgdata PGPORT=5432
PGBIN=$(ls -d /usr/lib/postgresql/*/bin | head -1)
export PATH="$PATH:$PGBIN"

echo "[entry] 1/4 启动内置 PostgreSQL..."
mkdir -p "$PGDATA" && chown postgres:postgres "$PGDATA"
su postgres -c "$PGBIN/initdb -D $PGDATA --auth=trust -U artex" >/dev/null
su postgres -c "$PGBIN/pg_ctl -D $PGDATA -o '-p $PGPORT -c listen_addresses=127.0.0.1' -w start" >/dev/null
su postgres -c "$PGBIN/createdb -p $PGPORT -U artex artex" 2>/dev/null || true

echo "[entry] 2/4 启动 ARTEX(禁用流量记录代理,省内存——沙箱 16G 长跑)..."
export ARTEX_PG_DSN="postgres://artex@127.0.0.1:$PGPORT/artex?sslmode=disable"
/app/start.sh --proxy "" &
for i in $(seq 1 60); do
  curl -sf -m 2 http://127.0.0.1:8787/api/health >/dev/null 2>&1 && break
  sleep 1
done
curl -sf -m 2 http://127.0.0.1:8787/api/health >/dev/null || { echo "[entry] ARTEX 未就绪,退出"; exit 1; }

echo "[entry] 3/4 预置 txzc 网关 profile(api_key 走 \${TXZC_DEEPSEEK_API_KEY})..."
su postgres -c "$PGBIN/psql -p $PGPORT -U artex -d artex -v ON_ERROR_STOP=1" <<'SQL'
INSERT INTO llm_profiles (name, format, base_url, model, api_key, context_window_k, is_default, priority, pool_exclude, streaming)
VALUES ('txzc-deepseek-gw', 'openai', 'http://api.deepseek.com.tsecbench.gw/v1', 'deepseek-flash', '${TXZC_DEEPSEEK_API_KEY}', 1024, false, 0, true, true)
ON CONFLICT (name) DO NOTHING;
SQL
PROFILE_ID=$(su postgres -c "$PGBIN/psql -p $PGPORT -U artex -d artex -tAc \"SELECT id FROM llm_profiles WHERE name='txzc-deepseek-gw'\"")
echo "[entry] profile id=$PROFILE_ID"

echo "[entry] 4/4 编排器开跑(一题一任务)..."
export ARTEX_API=http://127.0.0.1:8787 ARTEX_JWT_KEY=/app/jwt.key TSEC_PROFILE_ID="$PROFILE_ID"
exec python3 /app/orchestrator.py
