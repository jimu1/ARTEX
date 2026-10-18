// TSecBench 答题工具集(Challenges API):比赛/彩排环境专用。
// 仅当 BENCHMARK_BASE_URL + BENCHMARK_TOKEN 注入进程时才注册进 worker 工具集——
// 普通 SRC 任务看不到这组工具,零污染。平台接口语义见官方 Challenges API 文档:
// GET /openapi/v1/challenges、POST .../start、POST .../submit、POST .../close。
// hint 接口刻意不封装:查看提示会按比例扣分,从工具层杜绝误用。

package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	actool "github.com/Autumn-27/norma/tool"
)

// tsecEnabled reports whether the benchmark env pair is present in this process.
func tsecEnabled() bool {
	return strings.TrimSpace(os.Getenv("BENCHMARK_BASE_URL")) != "" &&
		strings.TrimSpace(os.Getenv("BENCHMARK_TOKEN")) != ""
}

// tsecClient is a minimal HTTP client for the Challenges API.
type tsecClient struct {
	base  string
	token string
	hc    *http.Client
}

func newTsecClient() (*tsecClient, error) {
	base := strings.TrimRight(strings.TrimSpace(os.Getenv("BENCHMARK_BASE_URL")), "/")
	token := strings.TrimSpace(os.Getenv("BENCHMARK_TOKEN"))
	if base == "" || token == "" {
		return nil, fmt.Errorf("未配置 BENCHMARK_BASE_URL/BENCHMARK_TOKEN,答题工具不可用")
	}
	return &tsecClient{base: base, token: token, hc: &http.Client{Timeout: 20 * time.Second}}, nil
}

// call performs one API round; non-2xx responses are surfaced as tool errors
// carrying the platform's code/message so the agent can react (duplicate,
// invalid_state, challenge_not_found …).
func (c *tsecClient) call(ctx context.Context, method, path string, body any) (json.RawMessage, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rd)
	if err != nil {
		return nil, err
	}
	req.Header.Set("BENCHMARK_TOKEN", c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("请求答题平台失败: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		var e struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		}
		_ = json.Unmarshal(raw, &e)
		if e.Code == "" {
			e.Code = fmt.Sprintf("http_%d", resp.StatusCode)
		}
		return nil, fmt.Errorf("答题平台 %s: %s (%s)", e.Code, e.Message, resp.Status)
	}
	return json.RawMessage(raw), nil
}

// tsecChallengeTools returns the benchmark answer-API tools for the worker set.
// Registered only when the env pair is present (see tsecEnabled).
func (t *ToolSet) tsecChallengeTools() []actool.CoreTool {
	if !tsecEnabled() {
		return nil
	}
	return []actool.CoreTool{
		t.tsecListChallenges(),
		t.tsecStartChallenge(),
		t.tsecSubmitFlag(),
		t.tsecCloseChallenge(),
	}
}

func (t *ToolSet) tsecListChallenges() actool.CoreTool {
	return readTool("tsec_list_challenges",
		"列评测题目清单与作答进度(每题: unique_code/描述/难度/分值/flag总数/已正确数/容器状态/靶机地址)。container_addr 仅在 container_status==available 时非空——为空说明该题还没 start。",
		obj(map[string]any{}),
		func(ctx context.Context, _ json.RawMessage) (actool.Result, error) {
			c, err := newTsecClient()
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			raw, err := c.call(ctx, http.MethodGet, "/openapi/v1/challenges", nil)
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			return jsonResult(json.RawMessage(raw))
		})
}

func (t *ToolSet) tsecStartChallenge() actool.CoreTool {
	return writeTool("tsec_start_challenge",
		"启动指定题目的靶机容器并返回直连地址(container_addr)。同一时间最多 3 道题目处于启动状态,做完务必 tsec_close_challenge 释放坑位。",
		obj(map[string]any{"unique_code": str("题目唯一标识,来自 tsec_list_challenges")}, "unique_code"),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				UniqueCode string `json:"unique_code"`
			}
			if err := json.Unmarshal(in, &a); err != nil || strings.TrimSpace(a.UniqueCode) == "" {
				return actool.Errorf("unique_code 必填"), nil
			}
			c, err := newTsecClient()
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			raw, err := c.call(ctx, http.MethodPost, "/openapi/v1/challenges/start?unique_code="+url.QueryEscape(strings.TrimSpace(a.UniqueCode)), nil)
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			return jsonResult(json.RawMessage(raw))
		})
}

func (t *ToolSet) tsecSubmitFlag() actool.CoreTool {
	return writeTool("tsec_submit_flag",
		"提交 flag 判定。只在确信 flag 完整无误(含 flag{...} 全格式)时提交;同一 flag 重复提交返回 duplicate 不加分。一题可有多个 flag(total_flag_count),逐个提交直到 correct_flag_count==total_flag_count。",
		obj(map[string]any{
			"unique_code": str("题目唯一标识"),
			"flag":        str("完整 flag 字符串"),
		}, "unique_code", "flag"),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				UniqueCode string `json:"unique_code"`
				Flag       string `json:"flag"`
			}
			if err := json.Unmarshal(in, &a); err != nil || strings.TrimSpace(a.UniqueCode) == "" || strings.TrimSpace(a.Flag) == "" {
				return actool.Errorf("unique_code 与 flag 均必填"), nil
			}
			c, err := newTsecClient()
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			raw, err := c.call(ctx, http.MethodPost, "/openapi/v1/challenges/submit", map[string]string{
				"unique_code": strings.TrimSpace(a.UniqueCode),
				"flag":        strings.TrimSpace(a.Flag),
			})
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			return jsonResult(json.RawMessage(raw))
		})
}

func (t *ToolSet) tsecCloseChallenge() actool.CoreTool {
	return writeTool("tsec_close_challenge",
		"关闭指定题目的靶机容器并释放坑位(活跃题目上限 3)。该题打完、确认拿不到更多 flag、或决定放弃时调用。",
		obj(map[string]any{"unique_code": str("题目唯一标识")}, "unique_code"),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				UniqueCode string `json:"unique_code"`
			}
			if err := json.Unmarshal(in, &a); err != nil || strings.TrimSpace(a.UniqueCode) == "" {
				return actool.Errorf("unique_code 必填"), nil
			}
			c, err := newTsecClient()
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			raw, err := c.call(ctx, http.MethodPost, "/openapi/v1/challenges/close?unique_code="+url.QueryEscape(strings.TrimSpace(a.UniqueCode)), nil)
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			return jsonResult(json.RawMessage(raw))
		})
}
