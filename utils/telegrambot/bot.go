// Package telegrambot implements a single, panel-side Telegram command bot.
// Agents remain outbound-only and never receive Telegram credentials or commands.
package telegrambot

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	_ "time/tzdata"

	"github.com/komari-monitor/komari/database/clients"
	"github.com/komari-monitor/komari/database/models"
	"github.com/komari-monitor/komari/internal/config"
	"github.com/komari-monitor/komari/utils/messageSender"
	telegramprovider "github.com/komari-monitor/komari/utils/messageSender/telegram"
	"github.com/komari-monitor/komari/utils/notifier"
	"github.com/komari-monitor/komari/utils/officialtraffic"
	agent_runtime "github.com/komari-monitor/komari/web/agent"
)

const (
	defaultEndpoint = "https://api.telegram.org/bot"
	maxNodes        = 30
	nodePageSize    = 10
	autoDeleteDelay = 5 * time.Minute
)

var manager struct {
	sync.Mutex
	cancel      context.CancelFunc
	fingerprint string
}

type bot struct {
	baseURL         string
	chatID          int64
	threadID        int64
	allowedUsers    map[int64]struct{}
	location        *time.Location
	client          *http.Client
	commandTimezone string
}

type telegramUser struct {
	ID    int64 `json:"id"`
	IsBot bool  `json:"is_bot"`
}

type telegramChat struct {
	ID int64 `json:"id"`
}

type telegramMessage struct {
	MessageID       int64         `json:"message_id"`
	MessageThreadID int64         `json:"message_thread_id"`
	From            *telegramUser `json:"from"`
	Chat            telegramChat  `json:"chat"`
	Text            string        `json:"text"`
}

type callbackQuery struct {
	ID      string           `json:"id"`
	From    telegramUser     `json:"from"`
	Message *telegramMessage `json:"message"`
	Data    string           `json:"data"`
}

type update struct {
	UpdateID      int64            `json:"update_id"`
	Message       *telegramMessage `json:"message"`
	CallbackQuery *callbackQuery   `json:"callback_query"`
}

type apiResponse[T any] struct {
	OK          bool   `json:"ok"`
	Description string `json:"description"`
	Result      T      `json:"result"`
}

type inlineButton struct {
	Text         string `json:"text"`
	CallbackData string `json:"callback_data"`
}

type inlineKeyboard struct {
	InlineKeyboard [][]inlineButton `json:"inline_keyboard"`
}

// Reload stops the existing poller and starts one matching the active Telegram
// provider configuration. Calling Reload repeatedly with unchanged settings is safe.
func Reload() {
	enabled, err := config.GetAs[bool](config.NotificationEnabledKey, false)
	if err != nil || !enabled || !messageSender.IsProviderConfigured("telegram") {
		stopIfRunning()
		return
	}

	_, values, exists, err := messageSender.GetNotificationChannelConfiguration("telegram")
	if err != nil || !exists {
		log.Print("Telegram command menu disabled: cannot load configuration")
		stopIfRunning()
		return
	}
	var addition telegramprovider.Addition
	if err := messageSender.DecodeConfiguration(values, &addition); err != nil {
		log.Print("Telegram command menu disabled: invalid configuration")
		stopIfRunning()
		return
	}
	if !addition.CommandMenuEnabled {
		stopIfRunning()
		return
	}

	b, err := newBot(addition)
	if err != nil {
		log.Printf("Telegram command menu disabled: %v", err)
		stopIfRunning()
		return
	}
	fingerprint := configFingerprint(addition)

	manager.Lock()
	if manager.cancel != nil && manager.fingerprint == fingerprint {
		manager.Unlock()
		return
	}
	if manager.cancel != nil {
		manager.cancel()
	}
	ctx, cancel := context.WithCancel(context.Background())
	manager.cancel = cancel
	manager.fingerprint = fingerprint
	manager.Unlock()

	go b.run(ctx)
}

// Stop terminates the long poller during server shutdown.
func Stop() { stopIfRunning() }

func stopIfRunning() {
	manager.Lock()
	defer manager.Unlock()
	if manager.cancel != nil {
		manager.cancel()
	}
	manager.cancel = nil
	manager.fingerprint = ""
}

func configFingerprint(addition telegramprovider.Addition) string {
	b, _ := json.Marshal(addition)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func newBot(addition telegramprovider.Addition) (*bot, error) {
	if strings.TrimSpace(addition.BotToken) == "" {
		return nil, errors.New("bot token is empty")
	}
	chatID, err := strconv.ParseInt(strings.TrimSpace(addition.ChatID), 10, 64)
	if err != nil {
		return nil, fmt.Errorf("invalid Telegram chat ID: %w", err)
	}
	var threadID int64
	if strings.TrimSpace(addition.MessageThreadID) != "" {
		threadID, err = strconv.ParseInt(strings.TrimSpace(addition.MessageThreadID), 10, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid Telegram message thread ID: %w", err)
		}
	}
	allowed, err := parseIDs(addition.CommandAllowedUsers)
	if err != nil {
		return nil, fmt.Errorf("invalid command user allow-list: %w", err)
	}
	tz := strings.TrimSpace(addition.CommandTimezone)
	if tz == "" {
		tz = "Asia/Shanghai"
	}
	location, err := time.LoadLocation(tz)
	if err != nil {
		return nil, fmt.Errorf("invalid command timezone %q: %w", tz, err)
	}
	endpoint := strings.TrimRight(strings.TrimSpace(addition.Endpoint), "/")
	if endpoint == "" {
		endpoint = defaultEndpoint
	}
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New("Telegram endpoint must be an HTTPS URL without credentials, query or fragment")
	}
	return &bot{
		baseURL:         endpoint + addition.BotToken,
		chatID:          chatID,
		threadID:        threadID,
		allowedUsers:    allowed,
		location:        location,
		client:          &http.Client{Timeout: 40 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
		commandTimezone: tz,
	}, nil
}

func parseIDs(raw string) (map[int64]struct{}, error) {
	result := make(map[int64]struct{})
	for _, value := range strings.Split(raw, ",") {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		id, err := strconv.ParseInt(value, 10, 64)
		if err != nil || id <= 0 {
			return nil, fmt.Errorf("%q is not a positive numeric user ID", value)
		}
		result[id] = struct{}{}
	}
	return result, nil
}

func (b *bot) run(ctx context.Context) {
	if err := b.configureMenu(ctx); err != nil {
		log.Printf("Failed to register Telegram command menu: %v", err)
	}
	log.Printf("Telegram command menu started (timezone=%s)", b.commandTimezone)
	var offset int64
	// Discard commands queued while the panel was stopped. A negative offset asks
	// Telegram for only the newest pending update and confirms older updates.
	if pending, err := b.getUpdates(ctx, -1); err == nil && len(pending) > 0 {
		offset = pending[len(pending)-1].UpdateID + 1
	}
	for ctx.Err() == nil {
		updates, err := b.getUpdates(ctx, offset)
		if err != nil {
			if ctx.Err() != nil {
				break
			}
			log.Printf("Telegram command polling failed: %v", err)
			select {
			case <-ctx.Done():
				break
			case <-time.After(3 * time.Second):
			}
			continue
		}
		for _, item := range updates {
			if item.UpdateID >= offset {
				offset = item.UpdateID + 1
			}
			b.handleUpdate(ctx, item)
		}
	}
	log.Println("Telegram command menu stopped")
}

func (b *bot) authorized(chatID, threadID int64, user *telegramUser) bool {
	if chatID != b.chatID || user == nil || user.IsBot {
		return false
	}
	if b.threadID != 0 && threadID != b.threadID {
		return false
	}
	if len(b.allowedUsers) == 0 {
		// In a private chat the positive chat ID equals the user ID. Groups must
		// configure an explicit user allow-list.
		return b.chatID > 0 && user.ID == b.chatID
	}
	_, ok := b.allowedUsers[user.ID]
	return ok
}

func (b *bot) handleUpdate(ctx context.Context, item update) {
	if item.Message != nil && b.authorized(item.Message.Chat.ID, item.Message.MessageThreadID, item.Message.From) {
		b.handleCommand(ctx, item.Message.Text)
		return
	}
	callback := item.CallbackQuery
	if callback == nil || callback.Message == nil || !b.authorized(callback.Message.Chat.ID, callback.Message.MessageThreadID, &callback.From) {
		return
	}
	_ = b.answerCallback(ctx, callback.ID)
	b.handleCallback(ctx, callback)
}

func (b *bot) handleCommand(ctx context.Context, text string) {
	fields := strings.Fields(strings.TrimSpace(text))
	if len(fields) == 0 || !strings.HasPrefix(fields[0], "/") {
		return
	}
	command := strings.ToLower(strings.SplitN(fields[0], "@", 2)[0])
	selector := strings.TrimSpace(strings.Join(fields[1:], " "))
	switch command {
	case "/start":
		b.sendMainMenu(ctx)
	case "/help":
		_ = b.send(ctx, helpText(), mainMenuKeyboard())
	case "/nodes":
		b.sendNodes(ctx)
	case "/today", "/yesterday":
		b.sendRange(ctx, strings.TrimPrefix(command, "/"), selector)
	case "/cycle":
		b.sendCycle(ctx, selector)
	case "/total":
		b.sendTotal(ctx, selector)
	case "/alltotal":
		b.sendAllTotal(ctx)
	case "/remaining":
		b.sendRemaining(ctx, selector)
	case "/reset":
		b.sendReset(ctx, selector)
	case "/resetlist":
		b.sendResetList(ctx)
	case "/status":
		b.sendStatus(ctx, selector)
	case "/report":
		b.sendRange(ctx, "today", selector)
		b.sendCycle(ctx, selector)
	case "/dmitcal":
		b.sendDMITCalibration(ctx, selector)
	case "/noslacal":
		b.sendNoSLACalibration(ctx, selector)
	default:
		_ = b.send(ctx, "未知命令。使用 /help 查看菜单。", nil)
	}
}

func (b *bot) handleCallback(ctx context.Context, callback *callbackQuery) {
	data := callback.Data
	parts := strings.SplitN(data, ":", 2)
	if len(parts) != 2 {
		return
	}
	action, uuid := parts[0], parts[1]
	switch action {
	case "menu":
		b.showMenuPanel(ctx, callback, uuid)
	case "nodes":
		page, _ := strconv.Atoi(uuid)
		b.showNodesPage(ctx, callback, page)
	case "node":
		client, ok := findClient(uuid)
		if !ok {
			_ = b.send(ctx, "节点不存在或已被删除。", nil)
			return
		}
		b.showNodeCard(ctx, callback, client)
	case "today", "yesterday":
		b.sendRange(ctx, action, uuid)
	case "cycle":
		b.sendCycle(ctx, uuid)
	case "total":
		b.sendTotal(ctx, uuid)
	case "alltotal":
		b.sendAllTotal(ctx)
	case "remaining":
		b.sendRemaining(ctx, uuid)
	case "reset":
		b.sendReset(ctx, uuid)
	case "resetlist":
		b.sendResetList(ctx)
	case "status":
		b.sendStatus(ctx, uuid)
	case "report":
		b.sendRange(ctx, "today", uuid)
		b.sendCycle(ctx, uuid)
	}
}

func (b *bot) configureMenu(ctx context.Context) error {
	commands := []map[string]string{
		{"command": "start", "description": "启动 Komari 流量机器人"},
		{"command": "nodes", "description": "选择一台监控机器"},
		{"command": "today", "description": "查询每台机器今日流量"},
		{"command": "yesterday", "description": "查询每台机器昨日流量"},
		{"command": "cycle", "description": "查询当前周期累计流量"},
		{"command": "total", "description": "查询机器累计总流量"},
		{"command": "alltotal", "description": "查询所有机器累计总流量"},
		{"command": "remaining", "description": "查询机器剩余总流量"},
		{"command": "reset", "description": "查询每台机器流量重置日"},
		{"command": "resetlist", "description": "列出所有机器重置日设置"},
		{"command": "status", "description": "查询机器在线与运行状态"},
		{"command": "report", "description": "立即生成完整流量报告"},
		{"command": "dmitcal", "description": "校准 DMIT/NoSLA 流量"},
		{"command": "noslacal", "description": "校准 NoSLA 流量"},
		{"command": "help", "description": "查看命令使用说明"},
	}
	encoded, _ := json.Marshal(commands)
	scope, _ := json.Marshal(map[string]any{"type": "chat", "chat_id": b.chatID})
	if err := b.call(ctx, "setMyCommands", url.Values{
		"commands": {string(encoded)},
		"scope":    {string(scope)},
	}, nil); err != nil {
		return err
	}

	// Telegram exposes the blue command button in private chats when the chat
	// menu button is explicitly set to the commands type.
	if b.chatID > 0 {
		menuButton, _ := json.Marshal(map[string]string{"type": "commands"})
		if err := b.call(ctx, "setChatMenuButton", url.Values{
			"chat_id":     {strconv.FormatInt(b.chatID, 10)},
			"menu_button": {string(menuButton)},
		}, nil); err != nil {
			return err
		}
	}
	return nil
}

func (b *bot) getUpdates(ctx context.Context, offset int64) ([]update, error) {
	values := url.Values{
		"offset":          {strconv.FormatInt(offset, 10)},
		"timeout":         {"25"},
		"allowed_updates": {`["message","callback_query"]`},
	}
	var result []update
	if err := b.call(ctx, "getUpdates", values, &result); err != nil {
		return nil, err
	}
	return result, nil
}

func (b *bot) send(ctx context.Context, text string, keyboard *inlineKeyboard) error {
	_, err := b.sendMessage(ctx, text, keyboard)
	return err
}

func (b *bot) sendEphemeral(ctx context.Context, text string, keyboard *inlineKeyboard) error {
	message, err := b.sendMessage(ctx, text, keyboard)
	if err != nil {
		return err
	}
	if message.MessageID != 0 {
		b.scheduleDeleteMessage(message.MessageID, autoDeleteDelay)
	}
	return nil
}

func (b *bot) sendMessage(ctx context.Context, text string, keyboard *inlineKeyboard) (telegramMessage, error) {
	values := url.Values{
		"chat_id":    {strconv.FormatInt(b.chatID, 10)},
		"text":       {text},
		"parse_mode": {"HTML"},
	}
	if b.threadID != 0 {
		values.Set("message_thread_id", strconv.FormatInt(b.threadID, 10))
	}
	if keyboard != nil {
		encoded, _ := json.Marshal(keyboard)
		values.Set("reply_markup", string(encoded))
	}
	var message telegramMessage
	if err := b.call(ctx, "sendMessage", values, &message); err != nil {
		return telegramMessage{}, err
	}
	return message, nil
}

func (b *bot) deleteMessage(ctx context.Context, messageID int64) error {
	return b.call(ctx, "deleteMessage", url.Values{
		"chat_id":    {strconv.FormatInt(b.chatID, 10)},
		"message_id": {strconv.FormatInt(messageID, 10)},
	}, nil)
}

func (b *bot) scheduleDeleteMessage(messageID int64, delay time.Duration) {
	time.AfterFunc(delay, func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if err := b.deleteMessage(ctx, messageID); err != nil {
			log.Printf("Telegram auto-delete message %d failed: %v", messageID, err)
		}
	})
}

func (b *bot) editMessage(ctx context.Context, messageID int64, text string, keyboard *inlineKeyboard) error {
	values := url.Values{
		"chat_id":    {strconv.FormatInt(b.chatID, 10)},
		"message_id": {strconv.FormatInt(messageID, 10)},
		"text":       {text},
		"parse_mode": {"HTML"},
	}
	if keyboard != nil {
		encoded, _ := json.Marshal(keyboard)
		values.Set("reply_markup", string(encoded))
	}
	return b.call(ctx, "editMessageText", values, nil)
}

func (b *bot) editOrSend(ctx context.Context, callback *callbackQuery, text string, keyboard *inlineKeyboard) {
	if callback != nil && callback.Message != nil && callback.Message.MessageID != 0 {
		if err := b.editMessage(ctx, callback.Message.MessageID, text, keyboard); err == nil || strings.Contains(strings.ToLower(err.Error()), "message is not modified") {
			return
		}
	}
	_ = b.send(ctx, text, keyboard)
}

func (b *bot) answerCallback(ctx context.Context, id string) error {
	return b.call(ctx, "answerCallbackQuery", url.Values{"callback_query_id": {id}}, nil)
}

func (b *bot) call(ctx context.Context, method string, values url.Values, output any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, b.baseURL+"/"+method, strings.NewReader(values.Encode()))
	if err != nil {
		return errors.New("Telegram request failed")
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := b.client.Do(req)
	if err != nil {
		return errors.New("Telegram request failed")
	}
	defer resp.Body.Close()

	var raw json.RawMessage
	result := apiResponse[json.RawMessage]{Result: raw}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&result); err != nil {
		return errors.New("Telegram request failed")
	}
	if !result.OK || resp.StatusCode != http.StatusOK {
		if method == "editMessageText" && strings.Contains(strings.ToLower(result.Description), "message is not modified") {
			return errors.New("message is not modified")
		}
		return fmt.Errorf("Telegram API %s failed (HTTP %d)", method, resp.StatusCode)
	}
	if output != nil && len(result.Result) > 0 && string(result.Result) != "true" {
		if err := json.Unmarshal(result.Result, output); err != nil {
			return errors.New("Telegram request failed")
		}
	}
	return nil
}

func helpText() string {
	return `<b>🤖 Komari 流量助手</b>
━━━━━━━━━━━━━━
<b>📊 流量查询</b>
/today — 今日流量
/yesterday — 昨日流量
/cycle — 当前周期累计
/total — 累计总流量
/alltotal — 所有机器累计总流量
/remaining — 剩余总流量
/reset — 每台机器流量重置日
/resetlist — 所有机器重置日列表

<b>🖥 节点管理</b>
/nodes — 选择监控机器
/status — 在线与运行状态
/report — 立即生成完整报告

<b>🛠 DMIT 校准</b>
/dmitcal CORONA 1.07TB
/dmitcal TINY 33.12GB
/dmitcal NOSLA 83.52GB
/noslacal 83.52GB

<b>💡 使用提示</b>
命令后可附机器名称或 UUID；不填写时查询全部机器。`
}

func (b *bot) sendMainMenu(ctx context.Context) {
	_ = b.send(ctx, b.mainMenuText(), mainMenuKeyboard())
}

func (b *bot) showMenuPanel(ctx context.Context, callback *callbackQuery, panel string) {
	switch panel {
	case "main":
		b.editOrSend(ctx, callback, b.mainMenuText(), mainMenuKeyboard())
	case "traffic":
		b.editOrSend(ctx, callback, trafficMenuText(), trafficMenuKeyboard())
	case "help":
		b.editOrSend(ctx, callback, helpText(), mainMenuKeyboard())
	default:
		b.editOrSend(ctx, callback, b.mainMenuText(), mainMenuKeyboard())
	}
}

type menuStats struct {
	Total           int
	Online          int
	Limited         int
	ResetConfigured int
	Timezone        string
}

func (b *bot) mainMenuText() string {
	stats := menuStats{Timezone: b.commandTimezone}
	if stats.Timezone == "" && b.location != nil {
		stats.Timezone = b.location.String()
	}
	if stats.Timezone == "" {
		stats.Timezone = "Asia/Shanghai"
	}
	list, err := clients.GetAllClientBasicInfo()
	if err == nil {
		stats.Total = len(list)
		for _, client := range list {
			if isOnline(client.UUID) {
				stats.Online++
			}
			if client.TrafficLimit > 0 {
				stats.Limited++
			}
			if client.TrafficResetReported && client.TrafficResetDay > 0 {
				stats.ResetConfigured++
			}
		}
	}
	return renderMainMenuText(stats)
}

func renderMainMenuText(stats menuStats) string {
	return fmt.Sprintf(`<b>🤖 Komari 流量控制台</b>
━━━━━━━━━━━━━━
🟢 在线：<b>%d / %d</b>　📦 限额：<b>%d</b> 台
🗓 重置日：<b>%d</b> 台　🕒 时区：<code>%s</code>

请选择要查看的功能：
📊 流量统计：今日 / 昨日 / 周期 / 累计 / 剩余
🖥 机器列表：选择单台机器后查看详情
🗓 重置日列表：汇总已设置、推测和未启用的机器`, stats.Online, stats.Total, stats.Limited, stats.ResetConfigured, html.EscapeString(stats.Timezone))
}

func mainMenuKeyboard() *inlineKeyboard {
	return &inlineKeyboard{InlineKeyboard: [][]inlineButton{
		{{Text: "📊 流量统计", CallbackData: "menu:traffic"}, {Text: "🖥 机器列表", CallbackData: "nodes:0"}},
		{{Text: "🗓 重置日列表", CallbackData: "resetlist:all"}, {Text: "🟢 运行状态", CallbackData: "status:"}},
		{{Text: "📋 完整报告", CallbackData: "report:"}, {Text: "ℹ️ 帮助说明", CallbackData: "menu:help"}},
	}}
}

func trafficMenuText() string {
	return `<b>📊 流量统计</b>
━━━━━━━━━━━━━━
请选择要查询的统计范围。

优先使用 vnStat 周期统计；没有 vnStat 数据时，自动回落到探针记录。
“剩余总流量”会结合面板里设置的流量上限显示状态灯和进度。`
}

func trafficMenuKeyboard() *inlineKeyboard {
	return &inlineKeyboard{InlineKeyboard: [][]inlineButton{
		{{Text: "📊 今日全部", CallbackData: "today:"}, {Text: "📅 昨日全部", CallbackData: "yesterday:"}},
		{{Text: "🔄 当前周期", CallbackData: "cycle:"}, {Text: "📦 剩余总流量", CallbackData: "remaining:"}},
		{{Text: "📊 累计全部", CallbackData: "total:"}, {Text: "🌐 全部累计汇总", CallbackData: "alltotal:all"}},
		{{Text: "🗓 重置日列表", CallbackData: "resetlist:all"}, {Text: "📋 完整报告", CallbackData: "report:"}},
		{{Text: "⬅️ 返回主面板", CallbackData: "menu:main"}},
	}}
}

func (b *bot) sendNodes(ctx context.Context) {
	b.sendNodesPage(ctx, 0)
}

func (b *bot) sendNodesPage(ctx context.Context, page int) {
	list, err := clients.GetAllClientBasicInfo()
	if err != nil {
		_ = b.send(ctx, "读取节点失败。", nil)
		return
	}
	sortClients(list)
	if len(list) == 0 {
		_ = b.send(ctx, "当前没有已添加的监控节点。", nil)
		return
	}
	text, keyboard := nodesPagePanel(list, page)
	_ = b.send(ctx, text, keyboard)
}

func (b *bot) showNodesPage(ctx context.Context, callback *callbackQuery, page int) {
	list, err := clients.GetAllClientBasicInfo()
	if err != nil {
		b.editOrSend(ctx, callback, "读取节点失败。", mainMenuKeyboard())
		return
	}
	sortClients(list)
	if len(list) == 0 {
		b.editOrSend(ctx, callback, "当前没有已添加的监控节点。", mainMenuKeyboard())
		return
	}
	text, keyboard := nodesPagePanel(list, page)
	b.editOrSend(ctx, callback, text, keyboard)
}

func nodesPagePanel(list []models.Client, page int) (string, *inlineKeyboard) {
	totalPages := (len(list) + nodePageSize - 1) / nodePageSize
	if totalPages < 1 {
		totalPages = 1
	}
	if page < 0 {
		page = 0
	}
	if page >= totalPages {
		page = totalPages - 1
	}
	start := page * nodePageSize
	end := start + nodePageSize
	if end > len(list) {
		end = len(list)
	}

	rows := make([][]inlineButton, 0, (end-start+1)/2+4)
	rows = append(rows, []inlineButton{{Text: "🌐 所有机器累计总流量", CallbackData: "alltotal:all"}})
	rows = append(rows, []inlineButton{{Text: "🗓 所有机器重置日列表", CallbackData: "resetlist:all"}})
	for i := start; i < end; i += 2 {
		row := []inlineButton{{Text: truncateButtonText(onlineMark(list[i].UUID) + displayName(list[i])), CallbackData: "node:" + list[i].UUID}}
		if i+1 < end {
			row = append(row, inlineButton{Text: truncateButtonText(onlineMark(list[i+1].UUID) + displayName(list[i+1])), CallbackData: "node:" + list[i+1].UUID})
		}
		rows = append(rows, row)
	}
	if totalPages > 1 {
		nav := []inlineButton{}
		if page > 0 {
			nav = append(nav, inlineButton{Text: "⬅️ 上一页", CallbackData: fmt.Sprintf("nodes:%d", page-1)})
		}
		nav = append(nav, inlineButton{Text: fmt.Sprintf("%d/%d", page+1, totalPages), CallbackData: fmt.Sprintf("nodes:%d", page)})
		if page+1 < totalPages {
			nav = append(nav, inlineButton{Text: "下一页 ➡️", CallbackData: fmt.Sprintf("nodes:%d", page+1)})
		}
		rows = append(rows, nav)
	}
	rows = append(rows, []inlineButton{{Text: "⬅️ 返回主面板", CallbackData: "menu:main"}})

	text := fmt.Sprintf("<b>🖥 机器列表</b>\n━━━━━━━━━━━━━━\n🟢 在线　⚫ 离线\n第 %d/%d 页，共 %d 台。\n\n点击机器名称查看详情：", page+1, totalPages, len(list))
	return text, &inlineKeyboard{InlineKeyboard: rows}
}

func (b *bot) sendNodeCard(ctx context.Context, client models.Client) {
	text, keyboard := nodeCardPanel(client)
	_ = b.send(ctx, text, keyboard)
}

func (b *bot) showNodeCard(ctx context.Context, callback *callbackQuery, client models.Client) {
	text, keyboard := nodeCardPanel(client)
	b.editOrSend(ctx, callback, text, keyboard)
}

func nodeCardPanel(client models.Client) (string, *inlineKeyboard) {
	status := "⚫ 离线"
	if isOnline(client.UUID) {
		status = "🟢 在线"
	}
	limitText := "∞ 不限制"
	if client.TrafficLimit > 0 {
		limitText = fmt.Sprintf("%s（%s）", humanBytes(client.TrafficLimit), trafficLimitTypeLabel(client.TrafficLimitType))
	}
	resetText := "未启用"
	if client.TrafficResetReported && client.TrafficResetDay > 0 {
		resetText = fmt.Sprintf("每月 %d 日", client.TrafficResetDay)
		if client.TrafficResetDay > 28 {
			resetText += "（短月顺延）"
		}
	} else if !client.TrafficResetReported {
		resetText = "自动推测"
	}
	text := fmt.Sprintf("<b>🖥 %s</b>\n━━━━━━━━━━━━━━\n<b>状态</b>　%s\n<b>流量上限</b>　%s\n<b>重置日</b>　%s\n<b>UUID</b>　<code>%s</code>",
		html.EscapeString(displayName(client)),
		status,
		html.EscapeString(limitText),
		html.EscapeString(resetText),
		html.EscapeString(client.UUID),
	)
	keyboard := &inlineKeyboard{InlineKeyboard: [][]inlineButton{
		{{Text: "📊 今日流量", CallbackData: "today:" + client.UUID}, {Text: "📅 昨日流量", CallbackData: "yesterday:" + client.UUID}},
		{{Text: "🔄 当前周期", CallbackData: "cycle:" + client.UUID}, {Text: "🗓 重置日", CallbackData: "reset:" + client.UUID}},
		{{Text: "📊 累计总流量", CallbackData: "total:" + client.UUID}, {Text: "📦 剩余总流量", CallbackData: "remaining:" + client.UUID}},
		{{Text: "🟢 运行状态", CallbackData: "status:" + client.UUID}, {Text: "📋 完整报告", CallbackData: "report:" + client.UUID}},
		{{Text: "⬅️ 返回机器列表", CallbackData: "nodes:0"}, {Text: "🏠 主面板", CallbackData: "menu:main"}},
	}}
	return text, keyboard
}

func (b *bot) sendRange(ctx context.Context, kind, selector string) {
	now := time.Now().In(b.location)
	start := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, b.location)
	totalLabel := "今日"
	end := now
	if kind == "yesterday" {
		end = start.Add(-time.Nanosecond)
		start = start.AddDate(0, 0, -1)
		totalLabel = "昨日"
	}
	list, err := selectClients(selector)
	if err != nil {
		_ = b.sendEphemeral(ctx, html.EscapeString(err.Error()), nil)
		return
	}
	for _, client := range list {
		totals, err := notifier.GetClientTrafficTotalsInRange(client.UUID, start, end)
		if err != nil {
			_ = b.sendEphemeral(ctx, formatTrafficErrorCard(client), nil)
			continue
		}
		usedVnstat := false
		if vnstatTotals, ok := notifier.GetClientVnstatRangeTotals(client, start, end); ok {
			totals = vnstatTotals
			usedVnstat = true
		}
		if kind == "today" && !usedVnstat {
			if latest, latestErr := notifier.GetLatestClientTrafficTotals(client.UUID); latestErr == nil {
				totals = capTrafficTotals(totals, latest)
			}
		}
		_ = b.sendEphemeral(ctx, formatTrafficCard(client, totals, totalLabel), nil)
	}
}

func (b *bot) sendCycle(ctx context.Context, selector string) {
	list, err := selectClients(selector)
	if err != nil {
		_ = b.sendEphemeral(ctx, html.EscapeString(err.Error()), nil)
		return
	}
	now := time.Now().In(b.location)
	for _, client := range list {
		if snapshot, ok := officialtraffic.GetSnapshot(client); ok {
			_ = b.sendEphemeral(ctx, formatOfficialTrafficCard(client, snapshot, "官方周期", b.location), nil)
			continue
		}
		if totals, ok := notifier.GetClientVnstatCycleTotals(client, now); ok {
			_ = b.sendEphemeral(ctx, formatTrafficCardWithNote(client, totals, "周期", officialTrafficFallbackNote(client)), nil)
			continue
		}
		b.sendCumulative(ctx, client.UUID, "周期")
	}
}

func (b *bot) sendTotal(ctx context.Context, selector string) {
	b.sendCumulative(ctx, selector, "累计")
}

func (b *bot) sendAllTotal(ctx context.Context) {
	list, err := clients.GetAllClientBasicInfo()
	if err != nil {
		_ = b.sendEphemeral(ctx, html.EscapeString(err.Error()), nil)
		return
	}
	sortClients(list)
	reports := agent_runtime.GetLatestReport()
	var up, down int64
	for _, client := range list {
		if totals, ok := notifier.GetClientVnstatLatestTotals(client); ok {
			up += totals.Up
			down += totals.Down
			continue
		}
		if report := reports[client.UUID]; report != nil {
			up += report.Network.TotalUp
			down += report.Network.TotalDown
			continue
		}
		if totals, queryErr := notifier.GetLatestClientTrafficTotals(client.UUID); queryErr == nil {
			up += totals.Up
			down += totals.Down
		}
	}
	_ = b.sendEphemeral(ctx, formatAllTrafficCard(len(list), up, down), nil)
}

func (b *bot) sendCumulative(ctx context.Context, selector, totalLabel string) {
	list, err := selectClients(selector)
	if err != nil {
		_ = b.sendEphemeral(ctx, html.EscapeString(err.Error()), nil)
		return
	}
	latest := agent_runtime.GetLatestReport()
	for _, client := range list {
		totals := notifier.TrafficTotals{}
		if totalLabel == "累计" {
			if vnstatTotals, ok := notifier.GetClientVnstatLatestTotals(client); ok {
				_ = b.sendEphemeral(ctx, formatTrafficCard(client, vnstatTotals, totalLabel), nil)
				continue
			}
		}
		if report := latest[client.UUID]; report != nil {
			totals.Up = report.Network.TotalUp
			totals.Down = report.Network.TotalDown
		} else {
			totals, err = notifier.GetLatestClientTrafficTotals(client.UUID)
			if err != nil {
				_ = b.sendEphemeral(ctx, formatTrafficErrorCard(client), nil)
				continue
			}
		}
		_ = b.sendEphemeral(ctx, formatTrafficCard(client, totals, totalLabel), nil)
	}
}

func (b *bot) sendReset(ctx context.Context, selector string) {
	list, err := selectClients(selector)
	if err != nil {
		_ = b.sendEphemeral(ctx, html.EscapeString(err.Error()), nil)
		return
	}
	now := time.Now().In(b.location)
	for _, client := range list {
		status := b.resetStatusForClient(client, now)
		_ = b.sendEphemeral(ctx, formatResetCard(client, status.Text), nil)
	}
}

func (b *bot) sendResetList(ctx context.Context) {
	list, err := clients.GetAllClientBasicInfo()
	if err != nil {
		_ = b.sendEphemeral(ctx, "读取节点失败。", nil)
		return
	}
	sortClients(list)
	if len(list) == 0 {
		_ = b.sendEphemeral(ctx, "当前没有已添加的监控节点。", nil)
		return
	}

	now := time.Now().In(b.location)
	counts := map[resetStatusKind]int{}
	lines := make([]string, 0, len(list))
	for _, client := range list {
		status := b.resetStatusForClient(client, now)
		counts[status.Kind]++
		lines = append(lines, formatResetListLine(client, status))
	}

	header := fmt.Sprintf("<b>🗓 流量重置日列表</b>\n━━━━━━━━━━━━━━\n✅ 已设置：%d　➖ 未启用：%d\n🧭 推测：%d　❔ 暂未检测：%d\n\n",
		counts[resetStatusConfigured],
		counts[resetStatusDisabled],
		counts[resetStatusInferred],
		counts[resetStatusUnknown]+counts[resetStatusError],
	)
	for _, message := range chunkTelegramMessage(header, lines) {
		_ = b.sendEphemeral(ctx, message, nil)
	}
}

type resetStatusKind int

const (
	resetStatusConfigured resetStatusKind = iota
	resetStatusDisabled
	resetStatusInferred
	resetStatusUnknown
	resetStatusError
)

type resetStatus struct {
	Kind resetStatusKind
	Text string
}

func (b *bot) resetStatusForClient(client models.Client, now time.Time) resetStatus {
	if client.TrafficResetReported {
		if client.TrafficResetDay == 0 {
			return resetStatus{Kind: resetStatusDisabled, Text: "未启用"}
		}
		note := ""
		if client.TrafficResetDay > 28 {
			note = "（短月顺延）"
		}
		return resetStatus{Kind: resetStatusConfigured, Text: fmt.Sprintf("每月 %d 日%s", client.TrafficResetDay, note)}
	}
	resetAt, found, err := notifier.GetLatestClientTrafficReset(client.UUID, now)
	if err != nil {
		return resetStatus{Kind: resetStatusError, Text: "查询失败"}
	}
	if found {
		return resetStatus{Kind: resetStatusInferred, Text: fmt.Sprintf("推测每月 %d 日", resetAt.In(b.location).Day())}
	}
	return resetStatus{Kind: resetStatusUnknown, Text: "暂未检测到"}
}

func (b *bot) sendRemaining(ctx context.Context, selector string) {
	list, err := selectClients(selector)
	if err != nil {
		_ = b.sendEphemeral(ctx, html.EscapeString(err.Error()), nil)
		return
	}
	reports := agent_runtime.GetLatestReport()
	for _, client := range list {
		if snapshot, ok := officialtraffic.GetSnapshot(client); ok {
			_ = b.sendEphemeral(ctx, formatOfficialTrafficCard(client, snapshot, "官方已用", b.location), nil)
			continue
		}
		totals := notifier.TrafficTotals{}
		if vnstatTotals, ok := notifier.GetClientVnstatCycleTotals(client, time.Now().In(b.location)); ok {
			totals = vnstatTotals
		} else if report := reports[client.UUID]; report != nil {
			totals.Up = report.Network.TotalUp
			totals.Down = report.Network.TotalDown
		} else if latest, queryErr := notifier.GetLatestClientTrafficTotals(client.UUID); queryErr == nil {
			totals = latest
		}
		used := notifier.ComputeUsedByType(strings.ToLower(client.TrafficLimitType), totals.Up, totals.Down)
		if client.TrafficLimit <= 0 {
			_ = b.sendEphemeral(ctx, withOfficialTrafficFallbackNote(formatRemainingCard(client, used, 0, 0, true), client), nil)
			continue
		}
		remaining := client.TrafficLimit - used
		if remaining < 0 {
			remaining = 0
		}
		_ = b.sendEphemeral(ctx, withOfficialTrafficFallbackNote(formatRemainingCard(client, used, remaining, client.TrafficLimit, false), client), nil)
	}
}

func (b *bot) sendStatus(ctx context.Context, selector string) {
	list, err := selectClients(selector)
	if err != nil {
		_ = b.sendEphemeral(ctx, html.EscapeString(err.Error()), nil)
		return
	}
	reports := agent_runtime.GetLatestReport()
	lines := []string{"<b>🖥 机器运行状态</b>\n━━━━━━━━━━━━━━"}
	for _, client := range list {
		state := "⚫ 离线"
		if isOnline(client.UUID) {
			state = "🟢 在线"
		}
		extra := ""
		if report := reports[client.UUID]; report != nil && report.Uptime > 0 {
			extra = " · 运行 " + formatDuration(time.Duration(report.Uptime)*time.Second)
		}
		lines = append(lines, fmt.Sprintf("%s <b>%s</b>%s", state, html.EscapeString(displayName(client)), extra))
	}
	_ = b.sendEphemeral(ctx, strings.Join(lines, "\n"), nil)
}

func selectClients(selector string) ([]models.Client, error) {
	list, err := clients.GetAllClientBasicInfo()
	if err != nil {
		return nil, err
	}
	sortClients(list)
	if strings.TrimSpace(selector) == "" {
		if len(list) > maxNodes {
			list = list[:maxNodes]
		}
		return list, nil
	}
	client, ok := findClientInList(list, selector)
	if !ok {
		return nil, fmt.Errorf("找不到节点 %q", selector)
	}
	return []models.Client{client}, nil
}

func findClient(selector string) (models.Client, bool) {
	list, err := clients.GetAllClientBasicInfo()
	if err != nil {
		return models.Client{}, false
	}
	return findClientInList(list, selector)
}

func findClientInList(list []models.Client, selector string) (models.Client, bool) {
	selector = strings.TrimSpace(selector)
	for _, client := range list {
		if client.UUID == selector || strings.EqualFold(client.Name, selector) {
			return client, true
		}
	}
	var match models.Client
	found := 0
	for _, client := range list {
		if strings.Contains(strings.ToLower(client.Name), strings.ToLower(selector)) {
			match = client
			found++
		}
	}
	return match, found == 1
}

func sortClients(list []models.Client) {
	sort.Slice(list, func(i, j int) bool {
		if list[i].Weight != list[j].Weight {
			return list[i].Weight > list[j].Weight
		}
		return strings.ToLower(displayName(list[i])) < strings.ToLower(displayName(list[j]))
	})
}

func displayName(client models.Client) string {
	if strings.TrimSpace(client.Name) != "" {
		return client.Name
	}
	return client.UUID
}

func truncateButtonText(text string) string {
	const maxRunes = 24
	runes := []rune(strings.TrimSpace(text))
	if len(runes) <= maxRunes {
		return string(runes)
	}
	return string(runes[:maxRunes-1]) + "…"
}

func onlineMark(uuid string) string {
	if isOnline(uuid) {
		return "🟢 "
	}
	return "⚫ "
}

func isOnline(uuid string) bool {
	for _, onlineUUID := range agent_runtime.GetAllOnlineUUIDs() {
		if onlineUUID == uuid {
			return true
		}
	}
	return false
}

func formatTrafficCard(client models.Client, totals notifier.TrafficTotals, totalLabel string) string {
	totals = capTodayTrafficTotals(client, totals, totalLabel)
	return notifier.FormatCompactTrafficCard(displayName(client), totalLabel, totals)
}

func formatTrafficCardWithNote(client models.Client, totals notifier.TrafficTotals, totalLabel, note string) string {
	card := formatTrafficCard(client, totals, totalLabel)
	if strings.TrimSpace(note) == "" {
		return card
	}
	return card + "\n" + note
}

func withOfficialTrafficFallbackNote(card string, client models.Client) string {
	note := officialTrafficFallbackNote(client)
	if strings.TrimSpace(note) == "" {
		return card
	}
	return card + "\n" + note
}

func officialTrafficFallbackNote(client models.Client) string {
	if !officialtraffic.IsConfiguredForUUID(client.UUID) {
		return ""
	}
	reason := "官方 API 暂不可用"
	if cachedReason, ok := officialtraffic.LastFailureReason(client.UUID); ok && strings.TrimSpace(cachedReason) != "" {
		reason = cachedReason
	}
	return "📡 官方源: 暂不可用，已回退探针数据\n⚠️ 原因: " + html.EscapeString(reason)
}

func capTodayTrafficTotals(client models.Client, totals notifier.TrafficTotals, totalLabel string) notifier.TrafficTotals {
	if totalLabel != "今日" || strings.TrimSpace(client.UUID) == "" {
		return totals
	}
	if latest, err := notifier.GetLatestClientTrafficTotals(client.UUID); err == nil {
		return capTrafficTotals(totals, latest)
	}
	return totals
}

func capTrafficTotals(totals, limit notifier.TrafficTotals) notifier.TrafficTotals {
	if limit.Up >= 0 && totals.Up > limit.Up {
		totals.Up = limit.Up
	}
	if limit.Down >= 0 && totals.Down > limit.Down {
		totals.Down = limit.Down
	}
	return totals
}

func formatTrafficErrorCard(client models.Client) string {
	return fmt.Sprintf("🖥️ 机器: <b>%s</b>\n⚠️ 流量统计失败", html.EscapeString(displayName(client)))
}

func formatAllTrafficCard(count int, up, down int64) string {
	return fmt.Sprintf("🖥️ 机器: <b>全部机器（%d 台）</b>\n━━━━━━━━━━━━━━\n🔼 上传: %s\n🔽 下载: %s\n📊 总计: <b>%s</b>", count, humanBytes(up), humanBytes(down), humanBytes(up+down))
}

func formatOfficialTrafficCard(client models.Client, snapshot *officialtraffic.Snapshot, totalLabel string, loc *time.Location) string {
	if snapshot == nil {
		return formatTrafficErrorCard(client)
	}
	if loc == nil {
		loc = time.Local
	}
	used := snapshot.UsedBytes
	limit := snapshot.LimitBytes
	remaining := snapshot.RemainingBytes
	percent := 0.0
	if limit > 0 {
		percent = float64(used) / float64(limit) * 100
	}
	statusIcon, statusText := trafficUsageStatus(percent)
	resetLine := ""
	if !snapshot.ResetAt.IsZero() {
		resetLine = "\n🔄 重置: " + html.EscapeString(snapshot.ResetAt.In(loc).Format("2006-01-02 15:04"))
	}
	sourceName := strings.TrimSpace(snapshot.SourceName)
	if sourceName == "" {
		sourceName = "官方 API"
	}
	limitText := "∞ 无限"
	if limit > 0 {
		limitText = humanBytes(limit)
	}
	remainingText := "∞ 无限"
	if limit > 0 {
		remainingText = humanBytes(remaining)
	}
	return fmt.Sprintf("🖥️ 机器: <b>%s</b>\n━━━━━━━━━━━━━━\n📡 来源: %s\n📊 %s: <b>%s</b> / %s\n📦 剩余: <b>%s</b>%s\n🎯 状态: %s %s　%.1f%%\n%s",
		html.EscapeString(displayName(client)),
		html.EscapeString(sourceName),
		html.EscapeString(totalLabel),
		humanBytes(used),
		limitText,
		remainingText,
		resetLine,
		statusIcon,
		statusText,
		percent,
		progressBar(percent),
	)
}

func formatRemainingCard(client models.Client, used, remaining, limit int64, unlimited bool) string {
	remainingText := humanBytes(remaining)
	limitText := humanBytes(limit)
	if unlimited {
		remainingText = "∞ 无限"
		limitText = "∞ 无限"
		return fmt.Sprintf("🖥️ 机器: <b>%s</b>\n━━━━━━━━━━━━━━\n📈 已用: %s\n📦 剩余: <b>%s</b>\n📊 总量: %s\n🎯 状态: ⚪ 未设置流量上限", html.EscapeString(displayName(client)), humanBytes(used), remainingText, limitText)
	}
	percent := 0.0
	if limit > 0 {
		percent = float64(used) / float64(limit) * 100
	}
	statusIcon, statusText := trafficUsageStatus(percent)
	return fmt.Sprintf("🖥️ 机器: <b>%s</b>\n━━━━━━━━━━━━━━\n📈 已用: %s / %s\n📦 剩余: <b>%s</b>\n🎯 状态: %s %s　%.1f%%\n%s", html.EscapeString(displayName(client)), humanBytes(used), limitText, remainingText, statusIcon, statusText, percent, progressBar(percent))
}

func formatResetCard(client models.Client, resetText string) string {
	return fmt.Sprintf("🖥️ 机器: <b>%s</b>\n━━━━━━━━━━━━━━\n🔄 重置: %s", html.EscapeString(displayName(client)), html.EscapeString(resetText))
}

func formatResetListLine(client models.Client, status resetStatus) string {
	icon := "❔"
	switch status.Kind {
	case resetStatusConfigured:
		icon = "✅"
	case resetStatusDisabled:
		icon = "➖"
	case resetStatusInferred:
		icon = "🧭"
	case resetStatusError:
		icon = "⚠️"
	}
	return fmt.Sprintf("%s <b>%s</b> — %s", icon, html.EscapeString(displayName(client)), html.EscapeString(status.Text))
}

func chunkTelegramMessage(header string, lines []string) []string {
	const maxTelegramTextLen = 3800
	if len(lines) == 0 {
		return []string{strings.TrimSpace(header)}
	}
	var chunks []string
	current := header
	for _, line := range lines {
		next := line
		if strings.TrimSpace(current) != "" {
			next = current + line + "\n"
		}
		if len(next) > maxTelegramTextLen && strings.TrimSpace(current) != "" {
			chunks = append(chunks, strings.TrimSpace(current))
			current = line + "\n"
			continue
		}
		current = next
	}
	if strings.TrimSpace(current) != "" {
		chunks = append(chunks, strings.TrimSpace(current))
	}
	return chunks
}

func humanBytes(value int64) string {
	if value < 0 {
		value = 0
	}
	const unit = 1024
	if value < unit {
		return fmt.Sprintf("%d B", value)
	}
	div, exp := int64(unit), 0
	for n := value / unit; n >= unit && exp < 5; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.2f %cB", float64(value)/float64(div), "KMGTPE"[exp])
}

func progressBar(percent float64) string {
	if percent < 0 {
		percent = 0
	}
	if percent > 100 {
		percent = 100
	}
	filled := int(percent/10 + 0.5)
	if filled > 10 {
		filled = 10
	}
	if filled < 0 {
		filled = 0
	}
	return strings.Repeat("▰", filled) + strings.Repeat("▱", 10-filled)
}

func trafficUsageStatus(percent float64) (string, string) {
	switch {
	case percent >= 100:
		return "🔴", "已用尽"
	case percent >= 90:
		return "🔴", "危险"
	case percent >= 75:
		return "🟡", "偏高"
	default:
		return "🟢", "安全"
	}
}

func trafficLimitTypeLabel(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "up":
		return "只算上传"
	case "down":
		return "只算下载"
	case "max":
		return "上传/下载取大"
	case "min":
		return "上传/下载取小"
	case "sum", "":
		return "上传+下载"
	default:
		return value
	}
}

func formatDuration(value time.Duration) string {
	days := int64(value / (24 * time.Hour))
	hours := int64(value/time.Hour) % 24
	if days > 0 {
		return fmt.Sprintf("%d天%d小时", days, hours)
	}
	minutes := int64(value/time.Minute) % 60
	return fmt.Sprintf("%d小时%d分钟", hours, minutes)
}
