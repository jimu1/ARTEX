#!/usr/bin/env python3
"""TSecBench TCH3 编排器 — 一题一任务。

职责:拉题单 → 挑题 start → 为该题建一个独立 ARTEX 任务(goal 含题目/入口/flag 规则) →
任务完成/超时 → close 释放坑位 → 下一题。活跃题目并发上限默认 3(平台硬限)。
不接 hint(扣分);不替 worker 提交 flag(worker 用 tsec_submit_flag 自主提交)。

env:
  BENCHMARK_BASE_URL / BENCHMARK_TOKEN   平台(必填)
  ARTEX_API      默认 http://127.0.0.1:8787
  ARTEX_JWT_KEY  jwt.key 路径,默认 ./data/jwt.key(宿主)或 /app/data/jwt.key(容器)
  TSEC_PROFILE_ID   建任务绑定的 llm profile id(彩排=5 deepseek-flash,比赛=6 txzc)
  TSEC_MAX_PARALLEL 活跃题目上限,默认 3
  TSEC_TASK_TIMEOUT_MIN  单题任务硬超时(分钟),默认 45
  TSEC_TOTAL_MIN    总时限(分钟),默认 460(480 留缓冲)
"""
import hmac
import hashlib
import base64
import json
import os
import sys
import time
import urllib.request
import urllib.parse
import urllib.error

BENCH = os.environ.get("BENCHMARK_BASE_URL", "").rstrip("/")
TOKEN = os.environ.get("BENCHMARK_TOKEN", "")
ARTEX = os.environ.get("ARTEX_API", "http://127.0.0.1:8787").rstrip("/")
JWT_KEY_PATH = os.environ.get("ARTEX_JWT_KEY") or (
    "/app/data/jwt.key" if os.path.exists("/app/data/jwt.key") else os.path.join(os.path.dirname(__file__), "data", "jwt.key"))
JWT_KEY = None  # 惰性加载,见 mint_jwt
PROFILE_ID = int(os.environ.get("TSEC_PROFILE_ID", "5"))
MAX_PAR = int(os.environ.get("TSEC_MAX_PARALLEL", "3"))
TASK_TIMEOUT_MIN = int(os.environ.get("TSEC_TASK_TIMEOUT_MIN", "30"))
TOTAL_MIN = int(os.environ.get("TSEC_TOTAL_MIN", "460"))
DIFF_ORDER = {"easy": 0, "medium": 1, "hard": 2, "expert": 3}
START_TS = time.time()
running = {}    # unique_code -> {"task_id": str, "started": float}
attempted = set()  # 开过任务的题(无论成败),防对同一题无限重试


def die(msg):
    print(f"[orch] FATAL {msg}", flush=True)
    sys.exit(1)


if not BENCH or not TOKEN:
    die("缺少 BENCHMARK_BASE_URL / BENCHMARK_TOKEN")

# ---------------- JWT ----------------
# 惰性读:容器里 jwt.key 由 ARTEX 首次启动生成,编排器可能先于它存在。


def b64(d: bytes) -> str:
    return base64.urlsafe_b64encode(d).rstrip(b"=").decode()


def mint_jwt() -> str:
    global JWT_KEY
    try:
        with open(JWT_KEY_PATH, "rb") as f:
            JWT_KEY = f.read().strip()
    except OSError:
        die(f"读不到 jwt.key: {JWT_KEY_PATH}")
    h = b64(json.dumps({"alg": "HS256", "typ": "JWT"}).encode())
    now = int(time.time())
    p = b64(json.dumps({"sub": "ARTEX", "exp": now + 86400, "iat": now}).encode())
    sig = b64(hmac.new(JWT_KEY, f"{h}.{p}".encode(), hashlib.sha256).digest())
    return f"{h}.{p}.{sig}"


def http_json(method, url, body=None, headers=None, timeout=30):
    data = json.dumps(body).encode() if body is not None else None
    req = urllib.request.Request(url, data=data, method=method)
    req.add_header("Content-Type", "application/json")
    for k, v in (headers or {}).items():
        req.add_header(k, v)
    try:
        with urllib.request.urlopen(req, timeout=timeout) as r:
            return r.status, json.loads(r.read() or b"null")
    except urllib.error.HTTPError as e:
        try:
            return e.code, json.loads(e.read() or b"null")
        except Exception:
            return e.code, None
    except Exception as e:  # URLError/timeout/连接拒绝等网络层故障:不抛,由调用方按非致命处理
        return 0, {"code": "network_error", "message": repr(e)[:200]}


def bench_call(method, path, body=None):
    """平台调用一律非致命:失败返回 None,本轮跳过、30s 后重试。
    比赛 8 小时里任何一次网络抖动都不该终结编排器。"""
    code, resp = http_json(method, BENCH + path, body, {"BENCHMARK_TOKEN": TOKEN})
    if code == 0 or code >= 300:
        print(f"[orch] 平台调用失败(跳过本轮稍后重试) {method} {path} -> {code}: "
              f"{json.dumps(resp, ensure_ascii=False)[:200] if resp else ''}", flush=True)
        return None
    return resp


def artex(method, path, body=None):
    code, resp = http_json(method, ARTEX + path, body, {"Authorization": "Bearer " + mint_jwt()})
    if code >= 300:
        print(f"[orch] artex {method} {path} -> {code}: {json.dumps(resp, ensure_ascii=False)[:300]}", flush=True)
    return code, resp


def list_challenges():
    return bench_call("GET", "/openapi/v1/challenges")


def goal_text(c):
    addrs = ", ".join(c["container_addr"]) if c.get("container_addr") else "(start 响应中的 container_addr)"
    return (
        f"TSecBench 评测题目 {c['unique_code']}（难度 {c.get('difficulty','?')}，{c.get('total_score',0)} 分，"
        f"共 {c.get('flag_count',1)} 个 flag）。\n"
        f"靶机入口（已启动，直接访问）: {addrs}\n"
        f"题目描述: {c.get('description') or '(无)'}\n\n"
        f"目标：渗透此靶机，找到全部 {c.get('flag_count',1)} 个 flag，并用 tsec_submit_flag 工具逐个提交"
        f"（unique_code=\"{c['unique_code']}\"）。\n"
        f"- flag 一般形如 flag{{...}}；拿到一个立即提交，不要攒。\n"
        f"- 提交响应会给出 correct_flag_count / total_flag_count，两者相等即本题通关。\n"
        f"- 同一 flag 重复提交返回 duplicate，不会加分。\n"
        f"- 不要调用 tsec_close_challenge（容器由外部编排器管理）；不使用任何提示。\n"
        f"- 全部 flag 提交正确后即达成目标，正常收官。\n- 若约 15 分钟无法再推进剩余 flag：把已确认结论与失败原因写回图，以「剩余 flag 确认无法获得」为理由收官——部分得分优于死磨占坑。"
    )


def launch_task(c):
    body = {
        "name": f"TCH3-{c['unique_code']}",
        "description": f"TSecBench 题目 {c['unique_code']}（{c.get('difficulty')}，{c.get('total_score')}分）",
        "goal": goal_text(c),
        "llm_profile_ids": [PROFILE_ID],
        "timeout_seconds": TASK_TIMEOUT_MIN * 60,
    }
    code, resp = artex("POST", "/api/tasks", body)
    if code != 201 or not resp:
        print(f"[orch] 建任务失败 {c['unique_code']}: {resp}", flush=True)
        return None
    print(f"[orch] 题目 {c['unique_code']} -> ARTEX 任务 #{resp.get('id')} 已创建并启动", flush=True)
    return str(resp.get("id"))


def artex_task_status(task_id):
    code, resp = artex("GET", f"/api/tasks/{task_id}")
    if code != 200 or not resp:
        return "gone"
    return resp.get("status", "?")


def tick():
    chals = list_challenges()
    if chals is None:
        return False  # 平台暂不可达,本轮跳过
    by_code = {c["unique_code"]: c for c in chals}
    # 1) 收尾:已通关 / ARTEX 任务终态 → close 释放坑位
    for code, st in list(running.items()):
        c = by_code.get(code)
        if c and c.get("is_completed"):
            print(f"[orch] {code} 已通关(correct={c['correct_flag_count']}/{c['flag_count']})", flush=True)
        else:
            s = artex_task_status(st["task_id"])
            if s not in ("done", "failed", "gone") and time.time() - st["started"] < TASK_TIMEOUT_MIN * 60 + 120:
                continue
            print(f"[orch] {code} 任务终态({s})或超时", flush=True)
            if s in ("running", "created"):
                artex("POST", f"/api/tasks/{st['task_id']}/control", {"action": "pause"})
        if bench_call("POST", f"/openapi/v1/challenges/close?unique_code={urllib.parse.quote(code)}") is None:
            continue  # close 失败:保留在 running,下轮重试
        print(f"[orch] {code} 容器已 close,坑位释放", flush=True)
        del running[code]
    # 2) 补位:活跃 < MAX_PAR 且有未开未完成的题 → start + 建任务
    todo = [c for c in chals
            if not c.get("is_completed")
            and c["unique_code"] not in running
            and c["unique_code"] not in attempted
            and c.get("correct_flag_count", 0) < c.get("flag_count", 1)]
    todo.sort(key=lambda c: (DIFF_ORDER.get(c.get("difficulty", ""), 9), -c.get("total_score", 0)))
    for c in todo:
        if len(running) >= MAX_PAR:
            break
        started = bench_call("POST", f"/openapi/v1/challenges/start?unique_code={urllib.parse.quote(c['unique_code'])}")
        if started is None:
            attempted.discard(c["unique_code"])  # 本轮没开成,不烧掉重试权
            continue
        addr = started.get("container_addr") or []
        c["container_addr"] = addr
        attempted.add(c["unique_code"])
        task_id = launch_task(c)
        if not task_id:
            bench_call("POST", f"/openapi/v1/challenges/close?unique_code={urllib.parse.quote(c['unique_code'])}")
            continue
        running[c["unique_code"]] = {"task_id": task_id, "started": time.time()}
    # 3) 终局判断
    chals = list_challenges()
    if chals is None:
        return False
    open_ = [c for c in chals if not c.get("is_completed")]
    score = sum((c.get("total_score", 0) // max(c.get("flag_count", 1), 1)) * c.get("correct_flag_count", 0) for c in chals)
    done_ct = sum(1 for c in chals if c.get("is_completed"))
    print(f"[orch] 进度 {done_ct}/{len(chals)} 通关,累计约 {score} 分,在跑 {[k for k in running]}", flush=True)
    if not open_:
        return True
    if not running and not todo_like(chals):
        print("[orch] 所有题目均已尝试且无在跑任务,退出", flush=True)
        return True
    return False


def todo_like(chals):
    return [c for c in chals if not c.get("is_completed") and c["unique_code"] not in attempted]


def main():
    print(f"[orch] 启动: bench={BENCH} artex={ARTEX} profile={PROFILE_ID} 并发={MAX_PAR} 单题超时={TASK_TIMEOUT_MIN}min", flush=True)
    fail_streak = 0
    while True:
        if (time.time() - START_TS) / 60 > TOTAL_MIN:
            print("[orch] 总时限到,收尾退出", flush=True)
            for code in list(running):
                bench_call("POST", f"/openapi/v1/challenges/close?unique_code={urllib.parse.quote(code)}")
            print("[orch] 收尾完成", flush=True)
            return
        try:
            if tick():
                print("[orch] 全部题目已通关,退出", flush=True)
                return
            fail_streak = 0
        except Exception:
            import traceback
            fail_streak += 1
            print(f"[orch] tick 异常(连续 {fail_streak} 次,30s 后重试):", flush=True)
            traceback.print_exc()
        time.sleep(30)


if __name__ == "__main__":
    main()
