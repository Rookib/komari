package client

import (
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/komari-monitor/komari/database/clients"
	"github.com/komari-monitor/komari/internal/config"
	"github.com/komari-monitor/komari/utils/geoip"
)

func getClientIPType(ip net.IP) int {
	// 0:ipv4 1:ipv6 -1:错误的输入
	if ip == nil {
		return -1
	}
	if ip.To4() == nil {
		return 1
	} else {
		return 0
	}
}

func saveClientBasicInfo(info map[string]interface{}, uuid string, fallbackIP string) error {
	// Only agent-owned telemetry fields may be updated. Account tokens, billing,
	// visibility and server-maintained vnStat anchors are administrator-owned.
	info = filterAgentBasicInfo(info)
	info["uuid"] = uuid
	if _, reported := info["traffic_reset_day"]; reported {
		info["traffic_reset_reported"] = true
		zone, _ := info["traffic_reset_timezone"].(string)
		zone = strings.TrimSpace(zone)
		if zone == "" {
			zone = "Local"
		}
		if len(zone) > 64 {
			return fmt.Errorf("traffic reset timezone is too long")
		}
		if _, err := time.LoadLocation(zone); err != nil {
			return fmt.Errorf("invalid traffic reset timezone %q: %w", zone, err)
		}
		info["traffic_reset_timezone"] = zone
	} else {
		delete(info, "traffic_reset_timezone")
		delete(info, "traffic_reset_reported")
	}
	applyFallbackClientIP(info, fallbackIP)
	appendClientRegionFromGeoIP(info)
	return clients.SaveClientInfo(info)
}

func applyFallbackClientIP(info map[string]interface{}, fallbackIP string) {
	if hasClientIP(info) {
		return
	}
	ip := net.ParseIP(fallbackIP)

	switch getClientIPType(ip) {
	case 0:
		info["ipv4"] = fallbackIP
	case 1:
		info["ipv6"] = fallbackIP
	}
}

func hasClientIP(info map[string]interface{}) bool {
	if ipv4, ok := info["ipv4"].(string); ok && ipv4 != "" {
		return true
	}
	if ipv6, ok := info["ipv6"].(string); ok && ipv6 != "" {
		return true
	}
	return false
}

func appendClientRegionFromGeoIP(info map[string]interface{}) {
	cfg, err := config.GetAs[bool](config.GeoIpEnabledKey)
	if err != nil || !cfg {
		return
	}

	for _, key := range []string{"ipv4", "ipv6"} {
		ipStr, ok := info[key].(string)
		if !ok || ipStr == "" {
			continue
		}
		ip := net.ParseIP(ipStr)
		if ip == nil {
			continue
		}
		record, _ := geoip.GetGeoInfo(ip)
		if record == nil {
			continue
		}
		region := geoip.GetRegionUnicodeEmoji(record.ISOCode)
		if region == "" {
			continue
		}
		info["region"] = region
		return
	}
}

func filterAgentBasicInfo(info map[string]interface{}) map[string]interface{} {
	allowed := []string{"cpu_name", "virtualization", "arch", "cpu_cores", "cpu_physical_cores",
		"os", "kernel_version", "gpu_name", "ipv4", "ipv6", "mem_total", "swap_total", "disk_total", "version",
		"traffic_reset_day", "traffic_reset_timezone", "vnstat_available", "vnstat_interface",
		"vnstat_total_up", "vnstat_total_down", "vnstat_days", "vnstat_updated_at"}
	filtered := make(map[string]interface{}, len(allowed))
	for _, key := range allowed {
		if value, ok := info[key]; ok {
			filtered[key] = value
		}
	}
	return filtered
}
