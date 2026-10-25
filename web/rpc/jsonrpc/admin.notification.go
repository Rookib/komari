package jsonrpc

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/komari-monitor/komari/database"
	"github.com/komari-monitor/komari/database/dbcore"
	"github.com/komari-monitor/komari/database/models"
	"github.com/komari-monitor/komari/database/notification"
	"github.com/komari-monitor/komari/database/sshlogin"
	"github.com/komari-monitor/komari/pkg/rpc"
	"github.com/komari-monitor/komari/utils/messageSender"
	"github.com/komari-monitor/komari/utils/telegrambot"
	"gorm.io/gorm/clause"
)

// admin.notification.go
// 通知相关 RPC2 方法（admin 命名空间）：离线通知、发送通知。

func init() {
	reg("listNotificationChannels", adminListNotificationChannels, "List notification channels")
	reg("getNotificationChannelConfiguration", adminGetNotificationChannelConfiguration, "Get notification channel configuration")
	reg("setNotificationChannelConfiguration", adminSetNotificationChannelConfiguration, "Set notification channel configuration")
	// offline notifications
	reg("listOfflineNotifications", adminListOfflineNotifications, "List offline notifications")
	reg("editOfflineNotification", adminEditOfflineNotification, "Edit offline notifications")
	reg("enableOfflineNotification", adminEnableOfflineNotification, "Enable offline notifications for clients")
	reg("disableOfflineNotification", adminDisableOfflineNotification, "Disable offline notifications for clients")
	// traffic report notifications
	reg("listTrafficReportNotifications", adminListTrafficReport, "List traffic report notifications")
	reg("editTrafficReportNotifications", adminEditTrafficReport, "Edit traffic report notifications")
	reg("enableTrafficReportNotifications", adminEnableTrafficReport, "Enable traffic report notifications")
	reg("disableTrafficReportNotifications", adminDisableTrafficReport, "Disable traffic report notifications")
	// SSH login notifications
	reg("listSSHLoginNotifications", adminListSSHLoginNotifications, "List SSH login notification settings")
	reg("editSSHLoginNotifications", adminEditSSHLoginNotifications, "Edit SSH login notification settings")
	reg("enableSSHLoginNotifications", adminEnableSSHLoginNotifications, "Enable SSH login notifications")
	reg("disableSSHLoginNotifications", adminDisableSSHLoginNotifications, "Disable SSH login notifications")
	reg("listSSHLoginEvents", adminListSSHLoginEvents, "List SSH login events")
	reg("deleteSSHLoginEvents", adminDeleteSSHLoginEvents, "Delete SSH login events")
	// send notification
	reg("sendNotification", adminSendNotification, "Send a notification")
}

func adminListNotificationChannels(_ context.Context, _ *rpc.JsonRpcRequest) (any, *rpc.JsonRpcError) {
	return messageSender.ListNotificationChannels(), nil
}

func adminGetNotificationChannelConfiguration(_ context.Context, req *rpc.JsonRpcRequest) (any, *rpc.JsonRpcError) {
	var params struct {
		ID string `json:"id"`
	}
	if err := req.BindParams(&params); err != nil {
		return nil, rpc.MakeError(rpc.InvalidParams, "Invalid request data: "+err.Error(), nil)
	}
	configuration, values, exists, err := messageSender.GetNotificationChannelConfiguration(params.ID)
	if err != nil {
		return nil, rpc.MakeError(rpc.InternalError, "Failed to load notification channel configuration: "+err.Error(), nil)
	}
	if !exists {
		return nil, rpc.MakeError(rpc.NotFound, "Notification channel not found: "+params.ID, nil)
	}
	return map[string]any{
		"configuration": configuration,
		"data":          values,
	}, nil
}

func adminSetNotificationChannelConfiguration(_ context.Context, req *rpc.JsonRpcRequest) (any, *rpc.JsonRpcError) {
	var params struct {
		ID   string         `json:"id"`
		Data map[string]any `json:"data"`
	}
	if err := req.BindParams(&params); err != nil {
		return nil, rpc.MakeError(rpc.InvalidParams, "Invalid request data: "+err.Error(), nil)
	}
	params.ID = strings.TrimSpace(params.ID)
	if !messageSender.NotificationChannelRegistered(params.ID) {
		return nil, rpc.MakeError(rpc.NotFound, "Notification channel not found: "+params.ID, nil)
	}
	if params.Data == nil {
		params.Data = map[string]any{}
	}
	addition, err := json.Marshal(params.Data)
	if err != nil {
		return nil, rpc.MakeError(rpc.InvalidParams, "Invalid notification channel configuration: "+err.Error(), nil)
	}
	if err := database.SaveMessageSenderConfig(&models.MessageSenderProvider{
		Name:     params.ID,
		Addition: string(addition),
	}); err != nil {
		return nil, rpc.MakeError(rpc.InternalError, "Failed to save notification channel configuration: "+err.Error(), nil)
	}
	if params.ID == "telegram" {
		telegrambot.Reload()
	}
	return nil, nil
}

// adminSendNotification 发送一条通知。仅供外部（插件/脚本）通过 RPC 调用，
// 内部通知逻辑（notifier/renewal/session 等）直接调用 messageSender.SendNotification。
func adminSendNotification(_ context.Context, req *rpc.JsonRpcRequest) (any, *rpc.JsonRpcError) {
	var params struct {
		Event models.EventMessage `json:"event"`
	}
	if err := req.BindParams(&params); err != nil {
		return nil, rpc.MakeError(rpc.InvalidParams, "Invalid request data: "+err.Error(), nil)
	}
	if fmt.Sprint(params.Event.Event) == "" {
		return nil, rpc.MakeError(rpc.InvalidParams, "event is required", nil)
	}
	if err := messageSender.SendNotification(params.Event); err != nil {
		return nil, rpc.MakeError(rpc.InternalError, "Failed to send notification: "+err.Error(), nil)
	}
	return nil, nil
}

// reg 是 admin 命名空间方法的注册便捷封装。
func reg(name string, h rpc.Handler, summary string) {
	RegisterWithGroupAndMeta(name, rpc.RoleAdmin, h, &rpc.MethodMeta{Name: "admin:" + name, Summary: summary})
}

func adminListOfflineNotifications(_ context.Context, _ *rpc.JsonRpcRequest) (any, *rpc.JsonRpcError) {
	var notifications []models.OfflineNotification
	if err := dbcore.GetDBInstance().Model(&models.OfflineNotification{}).Find(&notifications).Error; err != nil {
		return nil, rpc.MakeError(rpc.InternalError, "Failed to list offline notifications: "+err.Error(), nil)
	}
	return notifications, nil
}

func adminEditOfflineNotification(_ context.Context, req *rpc.JsonRpcRequest) (any, *rpc.JsonRpcError) {
	var notifications []models.OfflineNotification
	if err := req.BindParams(&notifications); err != nil {
		return nil, rpc.MakeError(rpc.InvalidParams, "Invalid request body: "+err.Error(), nil)
	}
	if len(notifications) == 0 {
		return nil, rpc.MakeError(rpc.InvalidParams, "At least one notification is required", nil)
	}
	for _, noti := range notifications {
		if noti.Client == "" {
			return nil, rpc.MakeError(rpc.InvalidParams, "Client UUID cannot be empty", nil)
		}
		if noti.GracePeriod <= 0 {
			return nil, rpc.MakeError(rpc.InvalidParams, "GracePeriod must be a positive integer", nil)
		}
	}
	err := dbcore.GetDBInstance().Model(&models.OfflineNotification{}).
		Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "client"}},
			DoUpdates: clause.AssignmentColumns([]string{"enable", "grace_period"}),
		}).
		Select("*").Create(notifications).Error
	if err != nil {
		return nil, rpc.MakeError(rpc.InternalError, "Failed to edit offline notifications: "+err.Error(), nil)
	}
	return nil, nil
}

// setOfflineNotificationEnable 是 enable/disable 的共享实现。
func setOfflineNotificationEnable(req *rpc.JsonRpcRequest, enable bool) *rpc.JsonRpcError {
	var uuids []string
	if err := req.BindParams(&uuids); err != nil {
		return rpc.MakeError(rpc.InvalidParams, "Invalid request body: "+err.Error(), nil)
	}
	notifications := make([]models.OfflineNotification, 0, len(uuids))
	for _, uuid := range uuids {
		notifications = append(notifications, models.OfflineNotification{Client: uuid, Enable: enable})
	}
	err := dbcore.GetDBInstance().Model(&models.OfflineNotification{}).
		Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "client"}},
			DoUpdates: clause.AssignmentColumns([]string{"enable"}),
		}).
		Select("client", "enable").Create(notifications).Error
	if err != nil {
		return rpc.MakeError(rpc.InternalError, "Failed to update offline notifications: "+err.Error(), nil)
	}
	return nil
}

func adminEnableOfflineNotification(_ context.Context, req *rpc.JsonRpcRequest) (any, *rpc.JsonRpcError) {
	if e := setOfflineNotificationEnable(req, true); e != nil {
		return nil, e
	}
	return nil, nil
}

func adminDisableOfflineNotification(_ context.Context, req *rpc.JsonRpcRequest) (any, *rpc.JsonRpcError) {
	if e := setOfflineNotificationEnable(req, false); e != nil {
		return nil, e
	}
	return nil, nil
}

func adminListTrafficReport(_ context.Context, _ *rpc.JsonRpcRequest) (any, *rpc.JsonRpcError) {
	list, err := notification.ListTrafficReportNotifications()
	if err != nil {
		return nil, rpc.MakeError(rpc.InternalError, "Failed to list traffic report notifications: "+err.Error(), nil)
	}
	return list, nil
}

func adminEditTrafficReport(_ context.Context, req *rpc.JsonRpcRequest) (any, *rpc.JsonRpcError) {
	var notifications []models.TrafficReportNotification
	if err := req.BindParams(&notifications); err != nil {
		return nil, rpc.MakeError(rpc.InvalidParams, "Invalid request body: "+err.Error(), nil)
	}
	if len(notifications) == 0 {
		return nil, rpc.MakeError(rpc.InvalidParams, "At least one notification is required", nil)
	}
	if err := notification.ValidateTrafficReportNotifications(notifications); err != nil {
		return nil, rpc.MakeError(rpc.InvalidParams, err.Error(), nil)
	}
	if err := notification.EditTrafficReportNotifications(notifications); err != nil {
		return nil, rpc.MakeError(rpc.InternalError, "Failed to edit traffic report notifications: "+err.Error(), nil)
	}
	return nil, nil
}

func adminEnableTrafficReport(_ context.Context, req *rpc.JsonRpcRequest) (any, *rpc.JsonRpcError) {
	var uuids []string
	if err := req.BindParams(&uuids); err != nil {
		return nil, rpc.MakeError(rpc.InvalidParams, "Invalid request body: "+err.Error(), nil)
	}
	if err := notification.EnableTrafficReportNotifications(uuids); err != nil {
		return nil, rpc.MakeError(rpc.InternalError, "Failed to enable traffic report notifications: "+err.Error(), nil)
	}
	return nil, nil
}

func adminDisableTrafficReport(_ context.Context, req *rpc.JsonRpcRequest) (any, *rpc.JsonRpcError) {
	var uuids []string
	if err := req.BindParams(&uuids); err != nil {
		return nil, rpc.MakeError(rpc.InvalidParams, "Invalid request body: "+err.Error(), nil)
	}
	if err := notification.DisableTrafficReportNotifications(uuids); err != nil {
		return nil, rpc.MakeError(rpc.InternalError, "Failed to disable traffic report notifications: "+err.Error(), nil)
	}
	return nil, nil
}

func adminListSSHLoginNotifications(_ context.Context, _ *rpc.JsonRpcRequest) (any, *rpc.JsonRpcError) {
	list, err := sshlogin.ListNotifications()
	if err != nil {
		return nil, rpc.MakeError(rpc.InternalError, "Failed to list SSH login notifications: "+err.Error(), nil)
	}
	return list, nil
}

func adminEditSSHLoginNotifications(_ context.Context, req *rpc.JsonRpcRequest) (any, *rpc.JsonRpcError) {
	var notifications []models.SSHLoginNotification
	if err := req.BindParams(&notifications); err != nil {
		return nil, rpc.MakeError(rpc.InvalidParams, "Invalid request body: "+err.Error(), nil)
	}
	if err := sshlogin.EditNotifications(notifications); err != nil {
		return nil, rpc.MakeError(rpc.InvalidParams, err.Error(), nil)
	}
	return nil, nil
}

func adminEnableSSHLoginNotifications(_ context.Context, req *rpc.JsonRpcRequest) (any, *rpc.JsonRpcError) {
	var uuids []string
	if err := req.BindParams(&uuids); err != nil {
		return nil, rpc.MakeError(rpc.InvalidParams, "Invalid request body: "+err.Error(), nil)
	}
	if err := sshlogin.SetNotificationEnable(uuids, true); err != nil {
		return nil, rpc.MakeError(rpc.InternalError, "Failed to enable SSH login notifications: "+err.Error(), nil)
	}
	return nil, nil
}

func adminDisableSSHLoginNotifications(_ context.Context, req *rpc.JsonRpcRequest) (any, *rpc.JsonRpcError) {
	var uuids []string
	if err := req.BindParams(&uuids); err != nil {
		return nil, rpc.MakeError(rpc.InvalidParams, "Invalid request body: "+err.Error(), nil)
	}
	if err := sshlogin.SetNotificationEnable(uuids, false); err != nil {
		return nil, rpc.MakeError(rpc.InternalError, "Failed to disable SSH login notifications: "+err.Error(), nil)
	}
	return nil, nil
}

func adminListSSHLoginEvents(_ context.Context, req *rpc.JsonRpcRequest) (any, *rpc.JsonRpcError) {
	var params struct {
		Client string `json:"client"`
		Limit  string `json:"limit"`
	}
	if err := req.BindParams(&params); err != nil {
		return nil, rpc.MakeError(rpc.InvalidParams, "Invalid request data", nil)
	}
	limit := 100
	if params.Limit != "" {
		parsed, err := strconv.Atoi(params.Limit)
		if err != nil {
			return nil, rpc.MakeError(rpc.InvalidParams, "Invalid limit", nil)
		}
		limit = parsed
	}
	events, err := sshlogin.ListEvents(params.Client, limit)
	if err != nil {
		return nil, rpc.MakeError(rpc.InternalError, "Failed to list SSH login events: "+err.Error(), nil)
	}
	return events, nil
}

func adminDeleteSSHLoginEvents(_ context.Context, req *rpc.JsonRpcRequest) (any, *rpc.JsonRpcError) {
	var params struct {
		Client string `json:"client"`
	}
	if err := req.BindParams(&params); err != nil {
		return nil, rpc.MakeError(rpc.InvalidParams, "Invalid request data", nil)
	}
	if err := sshlogin.DeleteEvents(params.Client); err != nil {
		return nil, rpc.MakeError(rpc.InternalError, "Failed to delete SSH login events: "+err.Error(), nil)
	}
	return nil, nil
}
