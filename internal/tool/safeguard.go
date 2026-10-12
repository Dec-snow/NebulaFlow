package tool

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
)

// 禁止访问的 IP 段：私有网络、回环、链路本地、云元数据等。
var blockedNetworks []*net.IPNet

func init() {
	cidrs := []string{
		// IPv4 私有地址
		"10.0.0.0/8",
		"172.16.0.0/12",
		"192.168.0.0/16",
		// IPv4 回环
		"127.0.0.0/8",
		// IPv4 链路本地（含云元数据 169.254.169.254）
		"169.254.0.0/16",
		// IPv6 唯一本地地址（ULA）
		"fc00::/7",
		// IPv6 回环
		"::1/128",
		// IPv6 链路本地
		"fe80::/10",
	}
	for _, cidr := range cidrs {
		_, n, err := net.ParseCIDR(cidr)
		if err != nil {
			panic(fmt.Sprintf("safeguard: invalid CIDR %q: %v", cidr, err))
		}
		blockedNetworks = append(blockedNetworks, n)
	}
}

// isURLAllowed 校验 URL 是否可安全请求，防止 SSRF。
//
// 校验规则：
//   - 仅允许 http/https scheme
//   - 拒绝 localhost / .local 等保留域名
//   - 仅允许标准端口 80 和 443
//   - 解析出的所有 IP 均不得落在禁止网段内
func isURLAllowed(rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("safeguard: invalid URL: %w", err)
	}

	// 1. scheme 校验
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("safeguard: disallowed scheme %q", u.Scheme)
	}

	host := u.Hostname()
	if host == "" {
		return errors.New("safeguard: empty hostname")
	}

	// 2. 端口校验：未指定端口时按 scheme 默认端口放行
	port := u.Port()
	if port != "" {
		if port != "80" && port != "443" {
			return fmt.Errorf("safeguard: disallowed port %s", port)
		}
	}

	// 3. 保留域名校验
	hostLower := strings.ToLower(host)
	if hostLower == "localhost" {
		return errors.New("safeguard: localhost is not allowed")
	}
	if strings.HasSuffix(hostLower, ".local") {
		return fmt.Errorf("safeguard: .local domain is not allowed: %s", host)
	}

	// 4. 解析 IP 并逐个校验
	ips, err := net.LookupIP(host)
	if err != nil {
		return fmt.Errorf("safeguard: failed to resolve %q: %w", host, err)
	}
	for _, ip := range ips {
		if isIPBlocked(ip) {
			return fmt.Errorf("safeguard: resolved IP %s is in blocked range", ip)
		}
	}

	return nil
}

// isIPBlocked 判断 IP 是否落在禁止网段中。
func isIPBlocked(ip net.IP) bool {
	for _, n := range blockedNetworks {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}
