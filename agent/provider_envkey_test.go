package agent

import "testing"

func TestConfigFromEnvKey(t *testing.T) {
	t.Setenv("ARTEX_TEST_KEY_SET", "sk-env-resolved")

	cases := []struct {
		name string
		key  string
		want string
	}{
		{"整串 ${VAR} 引用展开", "${ARTEX_TEST_KEY_SET}", "sk-env-resolved"},
		{"带空白仍识别", " ${ARTEX_TEST_KEY_SET} ", "sk-env-resolved"},
		{"未设置的变量展开为空(调用方按不可用处理)", "${ARTEX_TEST_KEY_UNSET_1}", ""},
		{"普通明文 key 原样通过", "sk-plain-key", "sk-plain-key"},
		{"嵌入引用不是整串形式,不展开", "prefix-${ARTEX_TEST_KEY_SET}", "prefix-${ARTEX_TEST_KEY_SET}"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := ConfigFrom("openai", "m", "https://x.example", tc.key, "")
			if c.APIKey != tc.want {
				t.Fatalf("api key = %q, want %q", c.APIKey, tc.want)
			}
		})
	}
}
