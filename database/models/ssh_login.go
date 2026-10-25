package models

import "time"

import (
	"net"
	"strings"
)

// SSHLoginNotification stores per-client SSH login notification settings.
// Missing rows are treated by the service layer as enabled for backwards
// compatibility with existing safe agents that already report SSH logins.
type SSHLoginNotification struct {
	Client      string      `json:"client" gorm:"type:varchar(36);not null;index;unique;constraint:OnDelete:CASCADE,OnUpdate:CASCADE;foreignKey:client;references:UUID"`
	ClientInfo  Client      `json:"client_info,omitempty" gorm:"foreignKey:Client;references:UUID"`
	Enable      bool        `json:"enable" gorm:"type:boolean;default:true"`
	IPWhitelist StringArray `json:"ip_whitelist" gorm:"type:longtext"`
}

func (n SSHLoginNotification) IsIPWhitelisted(ip string) bool {
	parsedIP := net.ParseIP(strings.TrimSpace(ip))
	if parsedIP == nil {
		return false
	}
	for _, entry := range n.IPWhitelist {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		if strings.Contains(entry, "/") {
			_, ipNet, err := net.ParseCIDR(entry)
			if err == nil && ipNet.Contains(parsedIP) {
				return true
			}
			continue
		}
		if candidate := net.ParseIP(entry); candidate != nil && candidate.Equal(parsedIP) {
			return true
		}
	}
	return false
}

// SSHLoginEvent records accepted SSH login events reported by an agent.
type SSHLoginEvent struct {
	ID          string    `json:"id" gorm:"type:varchar(36);primaryKey"`
	Client      string    `json:"client" gorm:"type:varchar(36);not null;index;constraint:OnDelete:CASCADE,OnUpdate:CASCADE;foreignKey:client;references:UUID"`
	ClientInfo  Client    `json:"client_info,omitempty" gorm:"foreignKey:Client;references:UUID"`
	User        string    `json:"user" gorm:"type:varchar(64);index"`
	RemoteIP    string    `json:"remote_ip" gorm:"type:varchar(100);index"`
	RemotePort  int       `json:"remote_port" gorm:"type:int"`
	AuthMethod  string    `json:"auth_method" gorm:"type:varchar(64)"`
	OccurredAt  time.Time `json:"occurred_at" gorm:"index"`
	CreatedAt   time.Time `json:"created_at"`
	Whitelisted bool      `json:"whitelisted" gorm:"type:boolean;default:false"`
}
