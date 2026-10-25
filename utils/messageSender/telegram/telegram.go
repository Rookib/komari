package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/komari-monitor/komari/utils/messageSender"
	"github.com/komari-monitor/komari/utils/messageSender/outboundhttp"
	"html"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type TelegramSender struct{ Addition }

func Register() error {
	return messageSender.RegisterNotificationChannel("telegram", messageSender.ManagedConfiguration("Telegram", Addition{}), &TelegramSender{})
}
func (t *TelegramSender) Unload() error { return nil }
func (t *TelegramSender) Send(ctx context.Context, n messageSender.Notification, values map[string]any) error {
	var addition Addition
	if err := messageSender.DecodeConfiguration(values, &addition); err != nil {
		return err
	}
	sender := TelegramSender{Addition: addition}
	return sender.send(ctx, n.Message, n.Title)
}
func (t *TelegramSender) send(ctx context.Context, message, title string) error {
	if strings.TrimSpace(t.BotToken) == "" || strings.TrimSpace(t.ChatID) == "" {
		return errors.New("Telegram is not fully configured")
	}
	fullMessage := html.EscapeString(message)
	if title != "" {
		fullMessage = fmt.Sprintf("<b>%s</b>\n%s", html.EscapeString(title), fullMessage)
	}
	if fullMessage == "" {
		return errors.New("message is empty")
	}
	endpoint := strings.TrimRight(strings.TrimSpace(t.Endpoint), "/")
	if endpoint == "" {
		endpoint = "https://api.telegram.org/bot"
	}
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return errors.New("Telegram endpoint must be an HTTPS URL without credentials, query or fragment")
	}
	data := url.Values{"chat_id": {t.ChatID}, "text": {fullMessage}, "parse_mode": {"HTML"}}
	if t.MessageThreadID != "" {
		data.Set("message_thread_id", t.MessageThreadID)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint+t.BotToken+"/sendMessage", strings.NewReader(data.Encode()))
	if err != nil {
		return errors.New("invalid Telegram request")
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	client := outboundhttp.NewClient(30 * time.Second)
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.Do(req)
	if err != nil {
		return errors.New("Telegram request failed")
	}
	defer resp.Body.Close()
	var result struct {
		OK bool `json:"ok"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&result); err != nil {
		return errors.New("invalid Telegram response")
	}
	if resp.StatusCode != http.StatusOK || !result.OK {
		return fmt.Errorf("Telegram rejected notification (HTTP %d)", resp.StatusCode)
	}
	return nil
}

var _ messageSender.NotificationChannel = (*TelegramSender)(nil)
