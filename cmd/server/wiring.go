package main

import (
	"fmt"
	"net"
	"strings"

	"github.com/lei-rr/workbuddy2api-pro/internal/pool"
	"github.com/lei-rr/workbuddy2api-pro/internal/server"
)

// realmAwareAvailableForModel 构造会话粘性路由按模型可用口径的 realm 感知闭包。
//
// 粘性分配的模型名可能带 realm 前缀（"global:gpt-5.4" / "cn:glm-5.2"）：必须按前缀剥出
// realm + bareModel，再交给分池选号域过滤——否则裸名取池子全集，global 号会被粘性分配给
// CN 前缀请求（跨 realm 泄漏）。裸名/显式 cn → cn 集合；global: → global 集合。
//
// realm 为空串时 pool.WeightedAvailableUIDsForModelRealm 退化为现状
// （AvailableUIDsForModel），老调用（无前缀模型名）语义零改动。
//
// 返回列表可能对快过期账号重复同一 UID，作为虚拟实例权重；会话哈希分配无需感知
// 权重细节，已有绑定的快路径仍直接返回原账号，不做迁移。
func realmAwareAvailableForModel(p *pool.Pool) func(model string) []string {
	return func(model string) []string {
		realm, bare := server.ResolveModel(model)
		return p.WeightedAvailableUIDsForModelRealm(bare, realm)
	}
}

// validateListenSecurity 启动安全校验（v2.0.1 #4-2「空 key 非回环阻断」）。
//
// 触发条件（三者同时成立即拒绝启动）：
//   - listen 的 host 不是回环地址（127.0.0.1 / ::1 / localhost；":7863" 与
//     "0.0.0.0:7863" 均视为绑定所有网卡，属非回环）；
//   - 网关 api_key 与面板 panel.api_key 都为空；
//   - security.allow_anonymous 未显式打开（默认 false）。
//
// 设计取舍：回环监听保留「免密钥本机自用」的合法场景；显式 allow_anonymous 是
// 逃生门（用户明确接受无鉴权，比如可信内网）。抽成纯函数便于单测，main 只负责
// log.Fatalf。
func validateListenSecurity(c *Config) error {
	if c == nil || c.Security.AllowAnonymous {
		return nil
	}
	if c.APIKey != "" || c.Panel.APIKey != "" {
		return nil
	}
	if isLoopbackListen(c.Listen) {
		return nil
	}
	return fmt.Errorf("listen=%q 绑定非回环地址，但 api_key 与 panel.api_key 均为空："+
		"网关与面板会完全无鉴权暴露在网络上（任何人可调用 /v1/* 并管理面板）。"+
		"请在 config.json 中设置 api_key 或 panel.api_key（随机密钥例如 `openssl rand -hex 24` 的输出；"+
		"删除 config.json 让程序重新生成时也会自动写入随机 api_key），"+
		"或设置 security.allow_anonymous=true 明确接受匿名访问，"+
		"或把 listen 改为 127.0.0.1:7863 仅本机访问", c.Listen)
}

// isLoopbackListen 判断监听地址是否只绑定回环网卡。
func isLoopbackListen(listen string) bool {
	host, _, err := net.SplitHostPort(strings.TrimSpace(listen))
	if err != nil {
		// 非 host:port 形态（理论上 normalize 后不会出现）：保守按非回环处理。
		return false
	}
	return isLoopbackHost(host)
}

// isLoopbackHost 判断 host 是否回环：空 host（":7863"）= 所有网卡，不算；
// localhost / 解析为 IP 且 IsLoopback 才算；其他主机名保守按非回环处理。
func isLoopbackHost(host string) bool {
	host = strings.TrimSpace(host)
	if host == "" {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false
}
