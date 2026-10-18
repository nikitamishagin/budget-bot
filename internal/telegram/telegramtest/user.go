package telegramtest

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"

	"github.com/nikitamishagin/budget-bot/internal/telegram"
)

// User drives a telegram.Bot the way a chat participant would, reading the bot's replies back
// from the FakeSender. The bot's Sender must be the same FakeSender.
type User struct {
	bot      *telegram.Bot
	sender   *FakeSender
	chatID   int64
	username string
	fileSeq  atomic.Int64
}

// NewUser creates a User bound to bot, sender, chatID and username.
func NewUser(bot *telegram.Bot, sender *FakeSender, chatID int64, username string) *User {
	return &User{bot: bot, sender: sender, chatID: chatID, username: username}
}

func (u *User) newFileID(kind string, data []byte) string {
	id := fmt.Sprintf("%s-%d-%d", kind, u.chatID, u.fileSeq.Add(1))
	u.sender.AddFile(id, data)
	return id
}

// SendDatabase uploads a document (typically the SQLite database) under the given file name.
func (u *User) SendDatabase(ctx context.Context, name string, data []byte) {
	u.bot.HandleUpdate(ctx, telegram.Update{
		ChatID:   u.chatID,
		Username: u.username,
		Document: &telegram.File{ID: u.newFileID("doc", data), Name: name},
	})
}

// SendPhoto uploads a screenshot with the given MIME type.
func (u *User) SendPhoto(ctx context.Context, data []byte, mime string) {
	u.bot.HandleUpdate(ctx, telegram.Update{
		ChatID:   u.chatID,
		Username: u.username,
		Photo:    &telegram.File{ID: u.newFileID("photo", data), MIME: mime},
	})
}

// SendCommand sends a bot command; a leading slash is optional ("/parse" or "parse").
func (u *User) SendCommand(ctx context.Context, cmd string) {
	u.bot.HandleUpdate(ctx, telegram.Update{
		ChatID:   u.chatID,
		Username: u.username,
		Command:  strings.TrimPrefix(cmd, "/"),
	})
}

// SendText sends a plain text message.
func (u *User) SendText(ctx context.Context, text string) {
	u.bot.HandleUpdate(ctx, telegram.Update{ChatID: u.chatID, Username: u.username, Text: text})
}

// Replies returns every text the bot has sent to this user's chat.
func (u *User) Replies() []string { return u.sender.Messages(u.chatID) }

// LastReply returns the most recent bot text for this chat, or "" if there is none.
func (u *User) LastReply() string {
	replies := u.Replies()
	if len(replies) == 0 {
		return ""
	}
	return replies[len(replies)-1]
}

// LastDocument returns the most recent document the bot sent to this chat.
func (u *User) LastDocument() (Document, bool) {
	docs := u.sender.Documents(u.chatID)
	if len(docs) == 0 {
		return Document{}, false
	}
	return docs[len(docs)-1], true
}
