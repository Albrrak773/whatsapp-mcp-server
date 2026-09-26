package main

import (
	"fmt"
	"strings"
	"time"

	"go.mau.fi/whatsmeow/proto/waE2E"
)

// Content is the normalized, human-readable view of a WhatsApp message payload.
type Content struct {
	Kind     string // text, image, video, gif, video_note, voice, audio, document, sticker, location, live_location, contact, poll, event, group_invite
	Text     string // body or caption, or a textual rendering for structured messages
	Mime     string
	FileName string
	Size     int64
	Seconds  int
	QuotedID string
	// Downloadable is true when the payload carries media keys that DownloadAny can fetch.
	Downloadable bool
}

// extractContent returns Kind == "" for payloads that are not user-visible chat messages
// (protocol messages, reactions, poll votes, key distribution, ...). Callers handle those separately.
func extractContent(m *waE2E.Message) Content {
	var c Content
	var ctx *waE2E.ContextInfo
	switch {
	case m.GetConversation() != "":
		c = Content{Kind: "text", Text: m.GetConversation()}
	case m.GetExtendedTextMessage() != nil:
		x := m.GetExtendedTextMessage()
		c = Content{Kind: "text", Text: x.GetText()}
		ctx = x.GetContextInfo()
	case m.GetImageMessage() != nil:
		x := m.GetImageMessage()
		c = Content{Kind: "image", Text: x.GetCaption(), Mime: x.GetMimetype(), Size: int64(x.GetFileLength()), Downloadable: true}
		ctx = x.GetContextInfo()
	case m.GetVideoMessage() != nil:
		x := m.GetVideoMessage()
		c = Content{Kind: "video", Text: x.GetCaption(), Mime: x.GetMimetype(), Size: int64(x.GetFileLength()),
			Seconds: int(x.GetSeconds()), Downloadable: true}
		if x.GetGifPlayback() {
			c.Kind = "gif"
		}
		ctx = x.GetContextInfo()
	case m.GetPtvMessage() != nil:
		x := m.GetPtvMessage()
		c = Content{Kind: "video_note", Mime: x.GetMimetype(), Size: int64(x.GetFileLength()),
			Seconds: int(x.GetSeconds()), Downloadable: true}
		ctx = x.GetContextInfo()
	case m.GetAudioMessage() != nil:
		x := m.GetAudioMessage()
		c = Content{Kind: "audio", Mime: x.GetMimetype(), Size: int64(x.GetFileLength()),
			Seconds: int(x.GetSeconds()), Downloadable: true}
		if x.GetPTT() {
			c.Kind = "voice"
		}
		ctx = x.GetContextInfo()
	case m.GetDocumentMessage() != nil:
		x := m.GetDocumentMessage()
		name := x.GetFileName()
		if name == "" {
			name = x.GetTitle()
		}
		c = Content{Kind: "document", Text: x.GetCaption(), Mime: x.GetMimetype(), FileName: name,
			Size: int64(x.GetFileLength()), Downloadable: true}
		ctx = x.GetContextInfo()
	case m.GetStickerMessage() != nil:
		x := m.GetStickerMessage()
		c = Content{Kind: "sticker", Mime: x.GetMimetype(), Size: int64(x.GetFileLength()), Downloadable: true}
		ctx = x.GetContextInfo()
	case m.GetLocationMessage() != nil:
		x := m.GetLocationMessage()
		c = Content{Kind: "location", Text: describePlace(x.GetName(), x.GetAddress(), x.GetDegreesLatitude(), x.GetDegreesLongitude())}
		ctx = x.GetContextInfo()
	case m.GetLiveLocationMessage() != nil:
		x := m.GetLiveLocationMessage()
		c = Content{Kind: "live_location", Text: describePlace(x.GetCaption(), "", x.GetDegreesLatitude(), x.GetDegreesLongitude())}
		ctx = x.GetContextInfo()
	case m.GetContactMessage() != nil:
		x := m.GetContactMessage()
		c = Content{Kind: "contact", Text: x.GetDisplayName() + vcardPhones(x.GetVcard())}
		ctx = x.GetContextInfo()
	case m.GetContactsArrayMessage() != nil:
		x := m.GetContactsArrayMessage()
		var parts []string
		for _, ct := range x.GetContacts() {
			parts = append(parts, ct.GetDisplayName()+vcardPhones(ct.GetVcard()))
		}
		c = Content{Kind: "contact", Text: strings.Join(parts, "\n")}
		ctx = x.GetContextInfo()
	case firstPoll(m) != nil:
		x := firstPoll(m)
		var b strings.Builder
		b.WriteString(x.GetName())
		for _, o := range x.GetOptions() {
			b.WriteString("\n- " + o.GetOptionName())
		}
		c = Content{Kind: "poll", Text: b.String()}
		ctx = x.GetContextInfo()
	case m.GetEventMessage() != nil:
		x := m.GetEventMessage()
		text := x.GetName()
		if x.GetStartTime() > 0 {
			text += " @ " + time.Unix(x.GetStartTime(), 0).Format(time.RFC3339)
		}
		if loc := x.GetLocation(); loc != nil {
			text += " (" + describePlace(loc.GetName(), loc.GetAddress(), loc.GetDegreesLatitude(), loc.GetDegreesLongitude()) + ")"
		}
		if d := x.GetDescription(); d != "" {
			text += "\n" + d
		}
		c = Content{Kind: "event", Text: text}
		ctx = x.GetContextInfo()
	case m.GetGroupInviteMessage() != nil:
		x := m.GetGroupInviteMessage()
		c = Content{Kind: "group_invite", Text: strings.TrimSpace(x.GetGroupName() + "\n" + x.GetCaption())}
		ctx = x.GetContextInfo()
	case m.GetButtonsResponseMessage() != nil:
		x := m.GetButtonsResponseMessage()
		c = Content{Kind: "text", Text: x.GetSelectedDisplayText()}
		ctx = x.GetContextInfo()
	case m.GetListResponseMessage() != nil:
		x := m.GetListResponseMessage()
		c = Content{Kind: "text", Text: x.GetTitle()}
		ctx = x.GetContextInfo()
	case m.GetTemplateButtonReplyMessage() != nil:
		x := m.GetTemplateButtonReplyMessage()
		c = Content{Kind: "text", Text: x.GetSelectedDisplayText()}
		ctx = x.GetContextInfo()
	case m.GetTemplateMessage().GetHydratedTemplate() != nil:
		x := m.GetTemplateMessage().GetHydratedTemplate()
		c = Content{Kind: "text", Text: strings.TrimSpace(x.GetHydratedTitleText() + "\n" + x.GetHydratedContentText())}
	case m.GetInteractiveMessage().GetBody() != nil:
		c = Content{Kind: "text", Text: m.GetInteractiveMessage().GetBody().GetText()}
	default:
		return Content{}
	}
	if ctx != nil {
		c.QuotedID = ctx.GetStanzaID()
	}
	return c
}

func firstPoll(m *waE2E.Message) *waE2E.PollCreationMessage {
	for _, p := range []*waE2E.PollCreationMessage{m.GetPollCreationMessage(), m.GetPollCreationMessageV2(),
		m.GetPollCreationMessageV3(), m.GetPollCreationMessageV5()} {
		if p != nil {
			return p
		}
	}
	return nil
}

func describePlace(name, address string, lat, lng float64) string {
	parts := []string{}
	if name != "" {
		parts = append(parts, name)
	}
	if address != "" {
		parts = append(parts, address)
	}
	parts = append(parts, fmt.Sprintf("https://maps.google.com/?q=%.6f,%.6f", lat, lng))
	return strings.Join(parts, ", ")
}

// vcardPhones pulls phone numbers out of a shared contact card.
func vcardPhones(vcard string) string {
	var phones []string
	for _, line := range strings.Split(vcard, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(strings.ToUpper(line), "TEL") {
			if i := strings.LastIndex(line, ":"); i >= 0 {
				phones = append(phones, strings.TrimSpace(line[i+1:]))
			}
		}
	}
	if len(phones) == 0 {
		return ""
	}
	return " (" + strings.Join(phones, ", ") + ")"
}
