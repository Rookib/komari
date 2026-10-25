package rpc

// IsDisabledAgentControl identifies upstream controls outside this distribution's
// TCP-safe policy. Fixed, validated firewall actions use a separate agent queue.
func IsDisabledAgentControl(method string) bool {
	switch method {
	case "admin:getAgentStartupConfig", "admin:switchAgentVersion":
		return true
	default:
		return false
	}
}
