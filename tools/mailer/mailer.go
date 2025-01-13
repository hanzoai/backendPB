package mailer

import (
	"io"
	"net/mail"

	"github.com/hanzoai/backendPB/tools/hook"
)

// Message defines a generic email message struct.
type Message struct {
	From              mail.Address         `json:"from"`
	To                []mail.Address       `json:"to"`
	Bcc               []mail.Address       `json:"bcc"`
	Cc                []mail.Address       `json:"cc"`
	Subject           string               `json:"subject"`
	HTML              string               `json:"html"`
	Text              string               `json:"text"`
	Headers           map[string]string    `json:"headers"`
	Attachments       map[string]io.Reader `json:"attachments"`
	InlineAttachments map[string]io.Reader `json:"inlineAttachments"`
}

// Mailer defines a base mail client interface.
type Mailer interface {
	// Send sends an email with the provided Message.
	Send(message *Message) error
}

// SendInterceptor is optional interface for registering mail send hooks.
type SendInterceptor interface {
	OnSend() *hook.Hook[*SendEvent]
}

type SendEvent struct {
	hook.Event
	Message *Message
}

// addressesToStrings converts the provided address to a list of serialized RFC 5322 strings.
//
// To export only the email part of mail.Address, you can set withName to false.
func addressesToStrings(addresses []mail.Address, withName bool) []string {
	result := make([]string, len(addresses))

	for i, addr := range addresses {
		if withName && addr.Name != "" {
			result[i] = addr.String()
		} else {
			// keep only the email part to avoid wrapping in angle-brackets
			result[i] = addr.Address
		}
	}

	return result
}
