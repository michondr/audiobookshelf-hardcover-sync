package email

import (
	"fmt"
	"net"
	"net/smtp"
	"time"
)

type Mailer struct {
	host string
	port string
	user string
	pass string
	from string
	to   string
}

func New(host, port, user, pass, from, to string) *Mailer {
	if from == "" {
		from = "abs-hc-sync@" + host
	}
	return &Mailer{host: host, port: port, user: user, pass: pass, from: from, to: to}
}

func (m *Mailer) Send(subject, body string) error {
	addr := net.JoinHostPort(m.host, m.port)
	msg := fmt.Sprintf(
		"From: %s\r\nTo: %s\r\nSubject: %s\r\nDate: %s\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n%s",
		m.from, m.to, subject, time.Now().Format(time.RFC1123Z), body,
	)

	var auth smtp.Auth
	if m.user != "" {
		auth = smtp.PlainAuth("", m.user, m.pass, m.host)
	}

	return smtp.SendMail(addr, auth, m.from, []string{m.to}, []byte(msg))
}
