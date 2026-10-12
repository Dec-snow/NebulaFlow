package tool

import (
	"strings"
	"testing"
)

func TestIsURLAllowed_PublicURL(t *testing.T) {
	// 正常公网 URL 应该通过
	err := isURLAllowed("https://example.com/")
	if err != nil {
		t.Fatalf("expected public URL to be allowed, got error: %v", err)
	}
}

func TestIsURLAllowed_Blocked(t *testing.T) {
	cases := []struct {
		name string
		url  string
		want string // 错误信息中应包含的关键词
	}{
		{"10.x.x.x 私有 IP", "http://10.0.0.1/", "blocked"},
		{"192.168.x.x 私有 IP", "http://192.168.1.1/", "blocked"},
		{"127.0.0.1 回环", "http://127.0.0.1/", "blocked"},
		{"169.254.169.254 云元数据", "http://169.254.169.254/", "blocked"},
		{"localhost", "http://localhost/", "localhost"},
		{"file:// scheme", "file:///etc/passwd", "scheme"},
		{"非 80/443 端口", "http://example.com:8080/", "port"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := isURLAllowed(c.url)
			if err == nil {
				t.Fatalf("expected error for %q, got nil", c.url)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("error %q does not contain %q", err.Error(), c.want)
			}
		})
	}
}
