package email

type Addition struct {
	Host         string `json:"host" required:"true"`
	Port         int    `json:"port" required:"true" default:"587"`
	Username     string `json:"username"`
	Password     string `json:"password"`
	Sender       string `json:"sender"`
	FromName     string `json:"from_name" help:"Optional display name used in the email From header, e.g. Komari Monitor"`
	Receiver     string `json:"receiver"`
	UseSSL       bool   `json:"use_ssl" default:"false"`
	UseLoginAuth bool   `json:"use_login_auth" default:"false" help:"Use LOGIN authentication method instead of PLAIN. Enable this if you encounter authentication errors with Microsoft (Outlook/Office365), NetEase (163.com), or other email providers"`
}
