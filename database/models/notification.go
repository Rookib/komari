package models

import "time"

// Notification 定义了通知相关的数据库模型
type OfflineNotification struct {
	Client     string `json:"client" gorm:"type:varchar(36);not null;index;unique;constraint:OnDelete:CASCADE,OnUpdate:CASCADE;foreignKey:client;references:UUID"`
	ClientInfo Client `json:"client_info,omitempty" gorm:"foreignKey:Client;references:UUID"`
	Enable     bool   `json:"enable" gorm:"type:boolean;default:false"`
	//Cooldown     int       `json:"cooldown" gorm:"type:int;not null;default:1800"`                // 冷却时间（秒），默认 30 分钟
	GracePeriod  int        `json:"grace_period" gorm:"type:int;not null;default:180"` // 宽限期（秒），默认 3 分钟
	LastNotified *time.Time `json:"last_notified"`                                     // 上次通知时间
}

// TrafficReportNotification 定义了流量定时报告的数据库模型
type TrafficReportNotification struct {
	Client     string `json:"client" gorm:"type:varchar(36);not null;index;unique;constraint:OnDelete:CASCADE,OnUpdate:CASCADE;foreignKey:client;references:UUID"`
	ClientInfo Client `json:"client_info,omitempty" gorm:"foreignKey:Client;references:UUID"`
	Enable     bool   `json:"enable" gorm:"type:boolean;default:false"`
	Daily      bool   `json:"daily" gorm:"type:boolean;default:false"`   // 日报
	Weekly     bool   `json:"weekly" gorm:"type:boolean;default:false"`  // 周报
	Monthly    bool   `json:"monthly" gorm:"type:boolean;default:false"` // 月报
}

// TrafficResetReminder records reset-day Telegram cards that have already been
// sent. The reset date is stored in the client's own traffic-reset timezone so
// panel restarts and hourly checks cannot duplicate the same day's reminder.
type TrafficResetReminder struct {
	Client     string    `json:"client" gorm:"type:varchar(36);primaryKey;not null;index;constraint:OnDelete:CASCADE,OnUpdate:CASCADE;foreignKey:client;references:UUID"`
	ClientInfo Client    `json:"client_info,omitempty" gorm:"foreignKey:Client;references:UUID"`
	ResetDate  string    `json:"reset_date" gorm:"type:char(10);primaryKey;not null"`
	SentAt     time.Time `json:"sent_at"`
}
