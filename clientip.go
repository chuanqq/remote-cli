package main

import (
	"context"
	"fmt"
	"net/http"
	"net/netip"
	"strings"
)

// parseTrustedProxies 只接受明确的 IP 或 CIDR，不进行 DNS 解析。
func parseTrustedProxies(value string) ([]netip.Prefix, error) {
	var prefixes []netip.Prefix
	for _, entry := range strings.Split(value, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		if addr, err := netip.ParseAddr(entry); err == nil && addr.Zone() == "" {
			addr = addr.Unmap()
			prefixes = append(prefixes, netip.PrefixFrom(addr, addr.BitLen()))
			continue
		}
		prefix, err := netip.ParsePrefix(entry)
		if err != nil {
			return nil, fmt.Errorf("invalid trusted proxy %q: expected an IP address or CIDR", entry)
		}
		if prefix.Addr().Is4In6() {
			if prefix.Bits() < 96 {
				return nil, fmt.Errorf("invalid trusted proxy %q: IPv4-mapped CIDR requires at least 96 prefix bits", entry)
			}
			prefix = netip.PrefixFrom(prefix.Addr().Unmap(), prefix.Bits()-96)
		}
		prefixes = append(prefixes, prefix.Masked())
	}
	return prefixes, nil
}

func trustedProxyStrings(prefixes []netip.Prefix) []string {
	var values []string
	for _, prefix := range prefixes {
		values = append(values, prefix.String())
	}
	return values
}

func isTrustedProxy(addr netip.Addr, prefixes []netip.Prefix) bool {
	for _, prefix := range prefixes {
		if prefix.Contains(addr) {
			return true
		}
	}
	return false
}

// peerClientIP 保留 TCP 对端作为兜底，同时统一 IPv4 和 IPv4-mapped IPv6。
func peerClientIP(remoteAddr string) (netip.Addr, string) {
	host := remoteHost(remoteAddr)
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}, host
	}
	addr = addr.WithZone("").Unmap()
	return addr, addr.String()
}

// forwardedIP 仅接受纯 IP；端口、zone、unknown 和带引号的值均不能作为来源地址。
func forwardedIP(value string) (netip.Addr, bool) {
	addr, err := netip.ParseAddr(strings.TrimSpace(value))
	if err != nil || addr.Zone() != "" {
		return netip.Addr{}, false
	}
	return addr.Unmap(), true
}

func resolveClientIP(r *http.Request, trusted []netip.Prefix) string {
	peerAddr, peer := peerClientIP(r.RemoteAddr)
	if !peerAddr.IsValid() || !isTrustedProxy(peerAddr, trusted) {
		return peer
	}

	if values := r.Header.Values("X-Forwarded-For"); len(values) > 0 {
		var chain []netip.Addr
		for _, value := range values {
			for _, hop := range strings.Split(value, ",") {
				addr, ok := forwardedIP(hop)
				if !ok {
					// 已存在但格式错误的 XFF 不能再由其他头覆盖。
					return peer
				}
				chain = append(chain, addr)
			}
		}
		// 从靠近服务器的一端开始，仅跨过明确可信的代理。
		for i := len(chain) - 1; i >= 0; i-- {
			if !isTrustedProxy(chain[i], trusted) || i == 0 {
				return chain[i].String()
			}
		}
		return peer
	}

	if values := r.Header.Values("X-Real-IP"); len(values) == 1 {
		if addr, ok := forwardedIP(values[0]); ok {
			return addr.String()
		}
	}
	return peer
}

type clientIPKey struct{}

// ClientIPMiddleware 在日志、限流之前解析一次；保留原始 RemoteAddr，不改写连接信息。
func ClientIPMiddleware(trusted []netip.Prefix, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := context.WithValue(r.Context(), clientIPKey{}, resolveClientIP(r, trusted))
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// clientIPFromRequest 在未经过生产中间件时也只回退到 TCP 对端，不直接读取请求头。
func clientIPFromRequest(r *http.Request) string {
	if ip, ok := r.Context().Value(clientIPKey{}).(string); ok {
		return ip
	}
	_, peer := peerClientIP(r.RemoteAddr)
	return peer
}
