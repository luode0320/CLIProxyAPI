package config

const (
	DefaultPanelGitHubRepository = "https://github.com/router-for-me/Cli-Proxy-API-Management-Center"
	DefaultPprofAddr             = "127.0.0.1:8316"
	DefaultAuthDir               = "~/.cli-proxy-api"

	// DefaultUpstreamResponseHeaderTimeout is the default number of seconds an
	// executor waits for upstream response headers before aborting. Chosen to be
	// generous enough for slow first-token generation while still surfacing a
	// silent upstream in well under a minute of downstream waiting.
	DefaultUpstreamResponseHeaderTimeout = 90
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
