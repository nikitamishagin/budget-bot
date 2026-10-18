package telegram

import (
	"context"

	"github.com/rs/zerolog/log"
)

// defaultPhotoMIME is used for photos whose update does not carry a MIME type.
const defaultPhotoMIME = "image/jpeg"

// File describes a user-uploaded file that can be downloaded through the Sender.
type File struct {
	ID   string
	Name string
	MIME string
}

// Update is a transport-neutral incoming message, so the bot can be driven without Telegram.
type Update struct {
	ChatID   int64
	Username string
	Text     string
	// Command is the bot command without the leading slash (e.g. "parse"); empty if none.
	Command  string
	Document *File
	Photo    *File
}

// HandleUpdate dispatches a single update to the appropriate Bot method, ignoring messages from
// users not listed in the bot's allowlist.
func (b *Bot) HandleUpdate(ctx context.Context, u Update) {
	if !b.IsAllowed(u.Username) {
		log.Warn().Str("username", u.Username).Msg("rejected message from unauthorized user")
		return
	}

	chatID := u.ChatID

	switch {
	case u.Command != "":
		log.Info().Str("username", u.Username).Int64("chat_id", chatID).Str("command", u.Command).Msg("user sent command")
		b.HandleCommand(ctx, chatID, u.Command)
	case u.Document != nil:
		log.Info().Str("username", u.Username).Int64("chat_id", chatID).Str("file_name", u.Document.Name).Msg("received file from user")
		data, err := b.Sender.DownloadDocument(u.Document.ID)
		if err != nil {
			log.Error().Err(err).Int64("chat_id", chatID).Msg("failed to download document")
			_ = b.Sender.SendMessage(chatID, "Sorry, I could not download that file.")
			return
		}
		b.HandleDocument(chatID, u.Document.Name, data)
	case u.Photo != nil:
		log.Info().Str("username", u.Username).Int64("chat_id", chatID).Msg("received screenshot from user")
		data, err := b.Sender.DownloadPhoto(u.Photo.ID)
		if err != nil {
			log.Error().Err(err).Int64("chat_id", chatID).Msg("failed to download photo")
			_ = b.Sender.SendMessage(chatID, "Sorry, I could not download that photo.")
			return
		}
		mime := u.Photo.MIME
		if mime == "" {
			mime = defaultPhotoMIME
		}
		b.HandlePhoto(ctx, chatID, data, mime)
	case u.Text != "":
		log.Info().Str("username", u.Username).Int64("chat_id", chatID).Str("reply", u.Text).Msg("received text reply from user")
		b.HandleText(ctx, chatID, u.Text)
	}
}

// Serve feeds updates into HandleUpdate until ctx is done or the channel is closed.
func (b *Bot) Serve(ctx context.Context, updates <-chan Update) {
	for {
		select {
		case <-ctx.Done():
			return
		case u, ok := <-updates:
			if !ok {
				return
			}
			b.HandleUpdate(ctx, u)
		}
	}
}
