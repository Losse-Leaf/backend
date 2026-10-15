package handlers

import (
	"fmt"
	"net"
	"net/url"
	"strings"

	"loose-leaf/internal/config"

	"github.com/gin-gonic/gin"
)

// NetworkCandidate 局域网或虚拟网卡接入端点候选
type NetworkCandidate struct {
	Type      string `json:"type"`       // "tailscale", "lan", "origin"
	Label     string `json:"label"`      // "Tailscale 节点", "局域网 Wi-Fi", etc.
	IP        string `json:"ip"`         // e.g. "100.112.108.52"
	URL       string `json:"url"`        // e.g. "http://100.112.108.52:8088"
	IsDefault bool   `json:"is_default"` // 是否被智能算法选为默认
}

// isLoopbackHost 检查地址或域名是否为本机回环 (localhost / 127.0.0.1 / ::1)
func isLoopbackHost(hostOrURL string) bool {
	raw := strings.TrimSpace(hostOrURL)
	if raw == "" {
		return true
	}
	if strings.Contains(raw, "://") {
		u, err := url.Parse(raw)
		if err == nil {
			raw = u.Hostname()
		}
	} else if idx := strings.Index(raw, ":"); idx != -1 {
		raw = raw[:idx]
	}
	raw = strings.Trim(raw, "[]")
	return raw == "localhost" || raw == "127.0.0.1" || raw == "::1" || raw == "0.0.0.0"
}

// isVirtualOrTunnelInterface 检查网卡是否为点对点 VPN 隧道、容器桥接或虚拟网卡 (非物理局域网)
func isVirtualOrTunnelInterface(name string, flags net.Flags) bool {
	// 点对点隧道网卡 (如 utun, ppp) 绝非局域网广播网卡
	if flags&net.FlagPointToPoint != 0 {
		return true
	}
	// 缺乏广播能力的网卡无法作为局域网通信
	if flags&net.FlagBroadcast == 0 {
		return true
	}

	nameLower := strings.ToLower(name)
	virtualPrefixes := []string{
		"utun", "tun", "tap", "docker", "br-", "veth",
		"vmnet", "vbox", "bridge", "awdl", "llw", "p2p", "ap",
	}
	for _, prefix := range virtualPrefixes {
		if strings.HasPrefix(nameLower, prefix) {
			return true
		}
	}
	return false
}

// DetectNetworkCandidates 扫描系统所有网卡，识别 Tailscale 虚拟网段与局域网 Wi-Fi
func DetectNetworkCandidates(serverPort string) []NetworkCandidate {
	port := strings.TrimSpace(serverPort)
	if port == "" {
		port = config.AppConfig.Port
	}
	if port == "" {
		port = "8088"
	}

	var candidates []NetworkCandidate
	seenIPs := make(map[string]bool)

	ifaces, err := net.Interfaces()
	if err != nil {
		return candidates
	}

	for _, iface := range ifaces {
		// 排除未启动或回环网卡
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}

		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}

		for _, addr := range addrs {
			var ip net.IP
			switch v := addr.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}
			ip4 := ip.To4()
			if ip4 == nil || ip4.IsLoopback() || ip4.IsLinkLocalUnicast() || ip4.IsLinkLocalMulticast() {
				continue
			}

			ipStr := ip4.String()
			candidateURL := fmt.Sprintf("http://%s:%s", ipStr, port)

			// 1. Tailscale 专属 CGNAT 范围: 100.64.0.0/10 (100.64.0.0 ~ 100.127.255.255)
			if ip4[0] == 100 && (ip4[1] >= 64 && ip4[1] <= 127) {
				if !seenIPs[ipStr] {
					seenIPs[ipStr] = true
					candidates = append(candidates, NetworkCandidate{
						Type:  "tailscale",
						Label: "Tailscale 节点",
						IP:    ipStr,
						URL:   candidateURL,
					})
				}
				continue
			}

			// 排除虚拟隧道、VPN、Docker 桥接或非广播网卡
			if isVirtualOrTunnelInterface(iface.Name, iface.Flags) {
				continue
			}

			// 2. 常见私有局域网 RFC 1918: 192.168.0.0/16, 10.0.0.0/8, 172.16.0.0/12
			isLan := false
			if ip4[0] == 192 && ip4[1] == 168 {
				isLan = true
			} else if ip4[0] == 10 {
				isLan = true
			} else if ip4[0] == 172 && (ip4[1] >= 16 && ip4[1] <= 31) {
				isLan = true
			}

			if isLan && !seenIPs[ipStr] {
				seenIPs[ipStr] = true
				candidates = append(candidates, NetworkCandidate{
					Type:  "lan",
					Label: "局域网 Wi-Fi",
					IP:    ipStr,
					URL:   candidateURL,
				})
			}
		}
	}

	return candidates
}

// ResolveSmartHubURL 智能避环解析中枢地址：
// 若调用方传入的是 localhost / 127.0.0.1，优先选择 Tailscale IP，其次局域网 Wi-Fi IP，确保移动设备扫码即可通
func ResolveSmartHubURL(incomingHubURL string, c *gin.Context) (string, []NetworkCandidate) {
	reqURL := strings.TrimRight(strings.TrimSpace(incomingHubURL), "/")
	if reqURL == "" {
		reqURL = resolveHubURL(c)
	}

	port := config.AppConfig.Port
	if port == "" {
		port = "8088"
	}

	candidates := DetectNetworkCandidates(port)

	// 若传入的地址并非 localhost（如公网域名 https://archive.example.com 或已有特定 IP），作为首选
	if !isLoopbackHost(reqURL) {
		resultCandidates := make([]NetworkCandidate, 0, len(candidates)+1)
		resultCandidates = append(resultCandidates, NetworkCandidate{
			Type:      "origin",
			Label:     "当前访问域名",
			IP:        reqURL,
			URL:       reqURL,
			IsDefault: true,
		})
		for _, cand := range candidates {
			cand.IsDefault = false
			resultCandidates = append(resultCandidates, cand)
		}
		return reqURL, resultCandidates
	}

	// 当前为回环地址 (localhost / 127.0.0.1)，按优先级进行智能避环
	bestIdx := -1
	// 优先级 1: Tailscale
	for i, cand := range candidates {
		if cand.Type == "tailscale" {
			bestIdx = i
			break
		}
	}
	// 优先级 2: 局域网 Wi-Fi
	if bestIdx == -1 {
		for i, cand := range candidates {
			if cand.Type == "lan" {
				bestIdx = i
				break
			}
		}
	}

	if bestIdx != -1 {
		for i := range candidates {
			candidates[i].IsDefault = (i == bestIdx)
		}
		return candidates[bestIdx].URL, candidates
	}

	// 无可用外部网卡时保底返回原始回环
	fallbackCandidate := NetworkCandidate{
		Type:      "origin",
		Label:     "本机回环",
		IP:        "127.0.0.1",
		URL:       reqURL,
		IsDefault: true,
	}
	return reqURL, []NetworkCandidate{fallbackCandidate}
}
