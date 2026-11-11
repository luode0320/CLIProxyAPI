package config

const (
	DefaultPanelGitHubRepository = "https://github.com/router-for-me/Cli-Proxy-API-Management-Center"
	DefaultPprofAddr             = "127.0.0.1:8316"
	DefaultAuthDir               = "~/.cli-proxy-api"

	// DefaultUpstreamResponseHeaderTimeout is the default number of seconds an
	// executor waits for upstream response headers before aborting. Chosen to be
	// generous enough for slow first-token generation while still surfacing a
	// silent upstream in well under a minute of downstream waiting.
	//
	// 2026-11-11 生产实测后从 90 提高到 180：
	// 大上下文请求的 prefill 会长时间不返回响应头，而这道闸门只限制「响应头到达
	// 前」的等待，与正文流式无关。原 90s 在 deepseek-v4-pro 上误杀了健康请求 ——
	// 近 7 天 304 个成功样本的 TTFT 最高 118.9s，其中 7 个（2.30%）超过 90s；
	// 同时该模型失败的 16 条里有 12 条耗时落在 85~160s 区间（p50=154.8s），
	// 正是这道超时的截断特征。
	//
	// 取 180 的依据：两模型历史最慢成功请求 123.6s，180s 留 45% 裕度，且
	// 309 个成功样本无一超过 180s（deepseek-v4-pro p95=79.0s、max=118.9s；
	// deepseek-v4.1-flash p95=6.4s、max=123.6s）。再高（如 300s）不增加任何
	// 覆盖，只会在真正挂死时多等 120s。需要关闭上限时显式配置 0。
	DefaultUpstreamResponseHeaderTimeout = 180
	DefaultDiscoveryServiceType          = "_ai-gateway._tcp"
)

// UpstreamResponseHeaderTimeoutSeconds resolves the effective upstream response
// header timeout in seconds. A nil config or nil field falls back to
// DefaultUpstreamResponseHeaderTimeout; an explicit 0 or negative value disables
// the cap and restores the previous unbounded behaviour.
func (c *Config) UpstreamResponseHeaderTimeoutSeconds() int {
	if c == nil || c.UpstreamResponseHeaderTimeout == nil {
		return DefaultUpstreamResponseHeaderTimeout
	}
	return *c.UpstreamResponseHeaderTimeout
}
