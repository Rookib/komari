package email

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"html"
	"mime"
	"mime/quotedprintable"
	"net"
	"net/mail"
	"net/smtp"
	"strconv"
	"strings"
	"time"

	"github.com/komari-monitor/komari/database/models"
	"github.com/komari-monitor/komari/utils/messageSender"
)

type EmailSender struct {
	Addition
}

// loginAuth supports LOGIN authentication only after the connection has
// completed certificate-verified TLS.
type loginAuth struct {
	username string
	password string
	host     string
}

func (a *loginAuth) Start(server *smtp.ServerInfo) (string, []byte, error) {
	if !server.TLS {
		return "", nil, fmt.Errorf("SMTP authentication requires TLS")
	}
	return "LOGIN", []byte{}, nil
}

func (a *loginAuth) Next(fromServer []byte, more bool) ([]byte, error) {
	if more {
		switch string(fromServer) {
		case "Username:", "User Name":
			return []byte(a.username), nil
		case "Password:", "password:":
			return []byte(a.password), nil
		default:
			// 某些服务器可能发送 base64 编码的提示
			// 尝试返回用户名或密码
			prompt := strings.ToLower(strings.TrimSpace(string(fromServer)))
			if strings.Contains(prompt, "user") {
				return []byte(a.username), nil
			}
			return []byte(a.password), nil
		}
	}
	return nil, nil
}

// plainAuthWithoutCheck retains compatibility with existing SMTP providers.
// The connection verifies the certificate hostname before this auth runs.
type plainAuthWithoutCheck struct {
	identity string
	username string
	password string
	host     string
}

func (a *plainAuthWithoutCheck) Start(server *smtp.ServerInfo) (string, []byte, error) {
	if !server.TLS {
		return "", nil, fmt.Errorf("SMTP authentication requires TLS")
	}
	resp := []byte(a.identity + "\x00" + a.username + "\x00" + a.password)
	return "PLAIN", resp, nil
}

func (a *plainAuthWithoutCheck) Next(fromServer []byte, more bool) ([]byte, error) {
	if more {
		return nil, fmt.Errorf("unexpected server challenge")
	}
	return nil, nil
}

func Register() error {
	return messageSender.RegisterNotificationChannel("email", messageSender.ManagedConfiguration("Email", Addition{}), &EmailSender{})
}
func (e *EmailSender) Unload() error { return nil }
func (e *EmailSender) Send(ctx context.Context, n messageSender.Notification, values map[string]any) error {
	var addition Addition
	if err := messageSender.DecodeConfiguration(values, &addition); err != nil {
		return err
	}
	sender := EmailSender{Addition: addition}
	if n.Event.Event == "" {
		return sender.sendMailContext(ctx, n.Message, "", n.Title)
	}
	return sender.sendMailContext(ctx, formatPlainEventEmail(n.Event), formatHTMLEventEmail(n.Event), n.Title)
}
func (e *EmailSender) SendTextMessage(message, title string) error {
	return e.sendMail(message, "", title)
}

func (e *EmailSender) SendEvent(event models.EventMessage) error {
	plain := formatPlainEventEmail(event)
	htmlBody := formatHTMLEventEmail(event)
	return e.sendMail(plain, htmlBody, eventText(event.Event))
}

func (e *EmailSender) sendMail(plainBody, htmlBody, title string) error {
	return e.sendMailContext(context.Background(), plainBody, htmlBody, title)
}
func (e *EmailSender) sendMailContext(ctx context.Context, plainBody, htmlBody, title string) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if e.Addition.Host == "" || e.Addition.Sender == "" || e.Addition.Username == "" || e.Addition.Password == "" || e.Addition.Receiver == "" {
		return fmt.Errorf("email sending is not fully configured")
	}

	// 使用更宽松的认证方式,优先尝试 PLAIN,如果失败则尝试 LOGIN
	// 这样可以兼容更多的 SMTP 服务器,包括微软邮箱、网易邮箱等
	var auth smtp.Auth
	if e.Addition.UseLoginAuth {
		// 使用 LOGIN 认证(适用于某些旧的或特殊的 SMTP 服务器)
		auth = &loginAuth{
			username: e.Addition.Username,
			password: e.Addition.Password,
			host:     e.Addition.Host,
		}
	} else {
		// 使用不检查主机名的 PLAIN 认证(适用于大多数现代 SMTP 服务器)
		auth = &plainAuthWithoutCheck{
			identity: "",
			username: e.Addition.Username,
			password: e.Addition.Password,
			host:     e.Addition.Host,
		}
	}

	// Parse sender address (for MAIL FROM and header). FromName only affects the
	// RFC 5322 header; SMTP envelope MAIL FROM must remain a plain email address.
	senderAddr, senderHeader := buildSenderAddress(e.Addition.Sender, e.Addition.FromName)

	// Parse recipients (support comma-separated list)
	var rcptList []string
	var rcptHeaderParts []string
	if addrs, err := mail.ParseAddressList(e.Addition.Receiver); err == nil {
		for _, a := range addrs {
			rcptList = append(rcptList, a.Address)
			rcptHeaderParts = append(rcptHeaderParts, a.String())
		}
	} else {
		// Fallback simple split
		parts := strings.Split(e.Addition.Receiver, ",")
		for _, p := range parts {
			p = strings.TrimSpace(p)
			if p == "" {
				continue
			}
			if a, err := mail.ParseAddress(p); err == nil {
				rcptList = append(rcptList, a.Address)
				rcptHeaderParts = append(rcptHeaderParts, a.String())
			} else {
				rcptList = append(rcptList, p)
				rcptHeaderParts = append(rcptHeaderParts, p)
			}
		}
	}
	if len(rcptList) == 0 {
		return fmt.Errorf("no valid recipient address parsed")
	}

	// RFC 2047 encode subject if non-ASCII
	encodedSubject := mime.QEncoding.Encode("UTF-8", title)

	// Compose headers
	headers := []string{
		"To: " + strings.Join(rcptHeaderParts, ", "),
		"From: " + senderHeader,
		"Subject: " + encodedSubject,
		"MIME-Version: 1.0",
		"Date: " + time.Now().Format(time.RFC1123Z),
		fmt.Sprintf("Message-ID: <%d@%s>", time.Now().UnixNano(), e.Addition.Host),
	}

	fullMsg, err := buildMIMEMessage(headers, plainBody, htmlBody)
	if err != nil {
		return err
	}

	addr := net.JoinHostPort(e.Host, strconv.Itoa(e.Port))
	dialer := &net.Dialer{Timeout: 30 * time.Second}
	var conn net.Conn
	if e.UseSSL && e.Port == 465 {
		conn, err = (&tls.Dialer{NetDialer: dialer, Config: &tls.Config{ServerName: e.Host, MinVersion: tls.VersionTLS12}}).DialContext(ctx, "tcp", addr)
	} else {
		conn, err = dialer.DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return fmt.Errorf("SMTP connection failed: %w", err)
	}
	defer conn.Close()
	deadline, _ := ctx.Deadline()
	if err = conn.SetDeadline(deadline); err != nil {
		return err
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	c, err := smtp.NewClient(conn, e.Host)
	if err != nil {
		return fmt.Errorf("SMTP greeting failed: %w", err)
	}
	defer c.Close()
	if !(e.UseSSL && e.Port == 465) {
		// Never transmit configured credentials on an unencrypted connection.
		// Retain the old UseSSL field for config compatibility; all other SMTP
		// connections require STARTTLS before authentication.
		if err = c.StartTLS(&tls.Config{ServerName: e.Host, MinVersion: tls.VersionTLS12}); err != nil {
			return fmt.Errorf("SMTP requires STARTTLS: %w", err)
		}
	}
	if err = c.Auth(auth); err != nil {
		return fmt.Errorf("SMTP authentication failed: %w", err)
	}
	if err = c.Mail(senderAddr); err != nil {
		return err
	}
	for _, rcpt := range rcptList {
		if err = c.Rcpt(rcpt); err != nil {
			return err
		}
	}
	w, err := c.Data()
	if err != nil {
		return err
	}
	if _, err = w.Write(fullMsg); err != nil {
		return err
	}
	if err = w.Close(); err != nil {
		return err
	}
	return c.Quit()
}

func buildSenderAddress(sender, fromName string) (string, string) {
	sender = strings.TrimSpace(sender)
	fromName = strings.TrimSpace(fromName)

	if addr, err := mail.ParseAddress(sender); err == nil {
		if fromName != "" {
			addr.Name = fromName
		}
		return addr.Address, addr.String()
	}

	if fromName == "" {
		return sender, sender
	}

	return sender, (&mail.Address{Name: fromName, Address: sender}).String()
}

// 确保实现了 IMessageSender 接口
var _ messageSender.NotificationChannel = (*EmailSender)(nil)

func buildMIMEMessage(headers []string, plainBody, htmlBody string) ([]byte, error) {
	if strings.TrimSpace(plainBody) == "" {
		plainBody = htmlToText(htmlBody)
	}
	if strings.TrimSpace(htmlBody) == "" {
		var bodyBuf bytes.Buffer
		if err := writeQuotedPrintable(&bodyBuf, plainBody); err != nil {
			return nil, err
		}
		msgHeaders := append([]string{}, headers...)
		msgHeaders = append(msgHeaders,
			"Content-Type: text/plain; charset=UTF-8",
			"Content-Transfer-Encoding: quoted-printable",
		)
		return []byte(strings.Join(msgHeaders, "\r\n") + "\r\n\r\n" + bodyBuf.String()), nil
	}

	boundary := fmt.Sprintf("komari-%d", time.Now().UnixNano())
	msgHeaders := append([]string{}, headers...)
	msgHeaders = append(msgHeaders, "Content-Type: multipart/alternative; boundary="+boundary)

	var body bytes.Buffer
	body.WriteString(strings.Join(msgHeaders, "\r\n"))
	body.WriteString("\r\n\r\n")

	body.WriteString("--" + boundary + "\r\n")
	body.WriteString("Content-Type: text/plain; charset=UTF-8\r\n")
	body.WriteString("Content-Transfer-Encoding: quoted-printable\r\n\r\n")
	if err := writeQuotedPrintable(&body, plainBody); err != nil {
		return nil, err
	}
	body.WriteString("\r\n")

	body.WriteString("--" + boundary + "\r\n")
	body.WriteString("Content-Type: text/html; charset=UTF-8\r\n")
	body.WriteString("Content-Transfer-Encoding: quoted-printable\r\n\r\n")
	if err := writeQuotedPrintable(&body, htmlBody); err != nil {
		return nil, err
	}
	body.WriteString("\r\n--" + boundary + "--\r\n")
	return body.Bytes(), nil
}

func writeQuotedPrintable(buf *bytes.Buffer, body string) error {
	qp := quotedprintable.NewWriter(buf)
	if _, err := qp.Write([]byte(body)); err != nil {
		return fmt.Errorf("failed to encode body: %w", err)
	}
	if err := qp.Close(); err != nil {
		return fmt.Errorf("failed to finalize body encoding: %w", err)
	}
	return nil
}

func formatPlainEventEmail(event models.EventMessage) string {
	return strings.TrimSpace(formatFallbackEventEmail(event))
}

func formatFallbackEventEmail(event models.EventMessage) string {
	clientText := eventClientText(event)
	switch eventText(event.Event) {
	case "SSH 登录成功":
		return strings.Join([]string{
			"🔐 SSH 安全登录提醒",
			"节点名称：" + clientText,
			eventText(event.Message),
			"✅ SSH 会话已成功建立",
		}, "\n")
	case "SSH 爆破告警":
		return strings.Join([]string{
			"🚨 SSH 爆破告警",
			eventText(event.Message),
			"该告警只代表检测到失败登录行为。",
		}, "\n")
	case "Offline":
		return "🔴 节点离线告警\n节点名称：" + clientText + "\n" + eventText(event.Message)
	case "Online":
		return "🟢 节点恢复在线\n节点名称：" + clientText + "\n" + eventText(event.Message)
	default:
		if strings.TrimSpace(eventText(event.Message)) != "" {
			return eventText(event.Event) + "\n节点名称：" + clientText + "\n" + eventText(event.Message)
		}
		return eventText(event.Event) + "\n节点名称：" + clientText
	}
}

func formatHTMLEventEmail(event models.EventMessage) string {
	clientText := eventClientText(event)
	fields := parseEventFields(eventText(event.Message))
	timeText := formatEmailEventTime(event.Time)

	switch eventText(event.Event) {
	case "SSH 爆破告警":
		return htmlCard(emailCardOptions{
			Accent:   "#f97316",
			Title:    "🚨 SSH 爆破告警",
			Subtitle: "检测到 SSH 登录失败 / 爆破行为",
			Fields: []emailField{
				{"节点名称", clientText},
				{"来源 IP", firstNonEmpty(fields["来源 IP"], fields["来源IP"])},
				{"目标用户", fields["目标用户"]},
				{"认证方式", fields["认证方式"]},
				{"失败次数", fields["失败次数"]},
				{"统计窗口", fields["统计窗口"]},
				{"时间", firstNonEmpty(fields["时间"], timeText)},
				{"封禁状态", firstNonEmpty(fields["封禁状态"], "未启用自动封禁")},
			},
			Footer:      "该告警只代表检测到失败登录行为。",
			FooterColor: "#fff7ed",
			FooterText:  "#9a3412",
		})
	case "SSH 登录成功":
		return htmlCard(emailCardOptions{
			Accent:   "#111827",
			Title:    "🔐 SSH 安全登录提醒",
			Subtitle: "检测到服务器成功建立 SSH 会话",
			Fields: []emailField{
				{"节点名称", clientText},
				{"登录账户", fields["登录账户"]},
				{"来源地址", fields["来源地址"]},
				{"登录终端", fields["登录终端"]},
				{"认证方式", fields["认证方式"]},
				{"登录时间", firstNonEmpty(fields["登录时间"], timeText)},
			},
			Footer:      "✅ SSH 会话已成功建立",
			FooterColor: "#ecfdf5",
			FooterText:  "#047857",
		})
	case "Offline":
		return htmlCard(emailCardOptions{
			Accent:   "#dc2626",
			Title:    "🔴 节点离线告警",
			Subtitle: "机器已离线，超过宽限期。",
			Fields: []emailField{
				{"节点名称", clientText},
				{"说明", eventText(event.Message)},
				{"时间", timeText},
			},
			Footer:      "请检查机器网络、系统负载或探针进程状态。",
			FooterColor: "#fef2f2",
			FooterText:  "#991b1b",
		})
	case "Online":
		return htmlCard(emailCardOptions{
			Accent:   "#16a34a",
			Title:    "🟢 节点恢复在线",
			Subtitle: "机器连接已恢复。",
			Fields: []emailField{
				{"节点名称", clientText},
				{"说明", eventText(event.Message)},
				{"时间", timeText},
			},
			Footer:      "节点当前已恢复在线。",
			FooterColor: "#ecfdf5",
			FooterText:  "#047857",
		})
	default:
		return htmlCard(emailCardOptions{
			Accent:   "#4f46e5",
			Title:    eventText(event.Event),
			Subtitle: "Komari 通知",
			Fields: []emailField{
				{"节点名称", clientText},
				{"说明", eventText(event.Message)},
				{"时间", timeText},
			},
			Footer:      "来自 Komari Monitor",
			FooterColor: "#eef2ff",
			FooterText:  "#3730a3",
		})
	}
}

type emailField struct {
	Label string
	Value string
}

type emailCardOptions struct {
	Accent      string
	Title       string
	Subtitle    string
	Fields      []emailField
	Footer      string
	FooterColor string
	FooterText  string
}

func htmlCard(opts emailCardOptions) string {
	var rows strings.Builder
	for _, field := range opts.Fields {
		value := strings.TrimSpace(field.Value)
		if value == "" {
			value = "-"
		}
		rows.WriteString(`<tr><td style="padding:9px 0;color:#6b7280;font-size:14px;white-space:nowrap;width:92px;vertical-align:top;">`)
		rows.WriteString(html.EscapeString(field.Label))
		rows.WriteString(`</td><td style="padding:9px 0;color:#111827;font-size:15px;font-weight:600;line-height:1.45;word-break:break-word;">`)
		rows.WriteString(html.EscapeString(value))
		rows.WriteString(`</td></tr>`)
	}

	return `<!doctype html><html><head><meta charset="UTF-8"><meta name="viewport" content="width=device-width, initial-scale=1.0"></head><body style="margin:0;padding:0;background:#f3f4f6;font-family:-apple-system,BlinkMacSystemFont,'Segoe UI',Roboto,'Helvetica Neue',Arial,'PingFang SC','Microsoft YaHei',sans-serif;color:#111827;"><table role="presentation" width="100%" cellspacing="0" cellpadding="0" style="width:100%;background:#f3f4f6;padding:18px 10px;"><tr><td align="center"><table role="presentation" cellspacing="0" cellpadding="0" style="width:100%;max-width:560px;background:#ffffff;border-radius:18px;overflow:hidden;border:1px solid #e5e7eb;box-shadow:0 8px 24px rgba(15,23,42,0.08);"><tr><td style="background:` + html.EscapeString(opts.Accent) + `;padding:18px 20px;color:#ffffff;"><div style="font-size:20px;font-weight:800;line-height:1.25;">` + html.EscapeString(opts.Title) + `</div><div style="font-size:13px;line-height:1.5;opacity:0.92;margin-top:5px;">` + html.EscapeString(opts.Subtitle) + `</div></td></tr><tr><td style="padding:18px 20px 8px 20px;"><table role="presentation" width="100%" cellspacing="0" cellpadding="0" style="width:100%;border-collapse:collapse;">` + rows.String() + `</table></td></tr><tr><td style="padding:8px 20px 20px 20px;"><div style="border-radius:12px;background:` + html.EscapeString(opts.FooterColor) + `;color:` + html.EscapeString(opts.FooterText) + `;padding:12px 14px;font-size:14px;font-weight:700;line-height:1.5;">` + html.EscapeString(opts.Footer) + `</div></td></tr></table></td></tr></table></body></html>`
}

func parseEventFields(message string) map[string]string {
	fields := make(map[string]string)
	for _, raw := range strings.Split(message, "\n") {
		line := strings.TrimSpace(raw)
		line = strings.TrimLeft(line, "🔐🚨🖥️📌👤🌐💻🔑🕒📈📦🎯⚠️✅ ")
		if line == "" || strings.HasPrefix(line, "说明") {
			continue
		}
		idx := strings.Index(line, "：")
		sepLen := len("：")
		if idx < 0 {
			idx = strings.Index(line, ":")
			sepLen = len(":")
		}
		if idx <= 0 {
			continue
		}
		key := strings.TrimSpace(line[:idx])
		value := strings.TrimSpace(line[idx+sepLen:])
		if key != "" && value != "" {
			fields[key] = value
		}
	}
	return fields
}

func eventClientText(event models.EventMessage) string {
	names := make([]string, 0, len(event.Clients))
	for _, client := range event.Clients {
		name := strings.TrimSpace(client.Name)
		if name == "" {
			name = strings.TrimSpace(client.UUID)
		}
		if name != "" {
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		return "-"
	}
	return strings.Join(names, ", ")
}

func formatEmailEventTime(t time.Time) string {
	if t.IsZero() {
		t = time.Now()
	}
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		loc = time.FixedZone("CST", 8*60*60)
	}
	return t.In(loc).Format("2006-01-02 15:04:05 北京时间")
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func htmlToText(body string) string {
	replacer := strings.NewReplacer("<br>", "\n", "<br/>", "\n", "<br />", "\n", "</p>", "\n", "</div>", "\n", "</tr>", "\n")
	text := replacer.Replace(body)
	var out strings.Builder
	inTag := false
	for _, r := range text {
		switch r {
		case '<':
			inTag = true
		case '>':
			inTag = false
		default:
			if !inTag {
				out.WriteRune(r)
			}
		}
	}
	return strings.TrimSpace(html.UnescapeString(out.String()))
}

func eventText(v any) string {
	if v == nil {
		return ""
	}
	return fmt.Sprint(v)
}
