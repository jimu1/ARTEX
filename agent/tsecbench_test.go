package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	actool "github.com/Autumn-27/norma/tool"
)

// TestTsecToolsEnvGate：未注入 BENCHMARK_* 时不注册任何工具；注入后恰好 4 个（无 hint）。
func TestTsecToolsEnvGate(t *testing.T) {
	tools := NewToolSet(nil, "worker")
	if got := tools.tsecChallengeTools(); len(got) != 0 {
		t.Fatalf("无环境变量时应返回空, got %d", len(got))
	}
	t.Setenv("BENCHMARK_BASE_URL", "http://127.0.0.1:1")
	t.Setenv("BENCHMARK_TOKEN", "tok")
	got := tools.tsecChallengeTools()
	if len(got) != 4 {
		t.Fatalf("注入后应注册 4 个工具, got %d", len(got))
	}
	for _, c := range got {
		switch c.Name() {
		case "tsec_list_challenges", "tsec_start_challenge", "tsec_submit_flag", "tsec_close_challenge":
		default:
			t.Fatalf("意外工具名: %s", c.Name())
		}
	}
}

// TestTsecToolsRoundtrip：httptest 假平台验证 submit 的鉴权头、请求体与错误/成功透传。
func TestTsecToolsRoundtrip(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("BENCHMARK_TOKEN")
		if r.URL.Path == "/openapi/v1/challenges/submit" && r.Method == http.MethodPost {
			var body struct{ Flag string `json:"flag"` }
			_ = json.NewDecoder(r.Body).Decode(&body)
			switch {
			case strings.Contains(body.Flag, "again"):
				w.WriteHeader(409)
				_, _ = w.Write([]byte(`{"code":"duplicate","message":"已提交过","detail":{}}`))
			case strings.Contains(body.Flag, "wrong"):
				_, _ = w.Write([]byte(`{"correct":false,"awarded":0,"matched_flag_index":null}`))
			default:
				_, _ = w.Write([]byte(`{"correct":true,"awarded":100,"cumulative_score":100,"correct_flag_count":1,"total_flag_count":2,"matched_flag_index":0}`))
			}
			return
		}
		_, _ = w.Write([]byte(`[]`))
	}))
	defer srv.Close()

	t.Setenv("BENCHMARK_BASE_URL", srv.URL)
	t.Setenv("BENCHMARK_TOKEN", "tok-123")

	byName := map[string]actool.CoreTool{}
	for _, tl := range NewToolSet(nil, "worker").tsecChallengeTools() {
		byName[tl.Name()] = tl
	}
	call := func(name, in string) string {
		t.Helper()
		res, err := byName[name].Call(context.Background(), json.RawMessage(in), nil)
		if err != nil {
			t.Fatalf("%s call: %v", name, err)
		}
		return res.Flatten()
	}

	if out := call("tsec_list_challenges", `{}`); !strings.Contains(out, "unique_code") && out != "[]" {
		t.Fatalf("list 输出异常: %s", out)
	}
	if out := call("tsec_start_challenge", `{"unique_code":"web_x"}`); !strings.Contains(out, "container_addr") && out != "[]" {
		t.Fatalf("start 输出异常: %s", out)
	}
	if out := call("tsec_submit_flag", `{"unique_code":"web_x","flag":"flag{good}"}`); !strings.Contains(out, `"correct":true`) {
		t.Fatalf("submit 正确路径异常: %s", out)
	}
	if out := call("tsec_submit_flag", `{"unique_code":"web_x","flag":"flag{wrong}"}`); !strings.Contains(out, `"correct":false`) {
		t.Fatalf("submit 错误 flag 应原样透传: %s", out)
	}
	if out := call("tsec_submit_flag", `{"unique_code":"web_x","flag":"flag{again}"}`); !strings.Contains(out, "duplicate") {
		t.Fatalf("duplicate 错误应透传 code: %s", out)
	}
	if out := call("tsec_close_challenge", `{"unique_code":"web_x"}`); out == "" {
		t.Fatal("close 应有输出")
	}
	if gotAuth != "tok-123" {
		t.Fatalf("鉴权头缺失: %q", gotAuth)
	}
}
