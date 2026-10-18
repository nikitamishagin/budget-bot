// Package telegramtest provides importable fakes for driving a telegram.Bot in tests without the
// real Telegram API: a recording FakeSender and a User that plays the part of a chat participant.
package telegramtest

import (
	"fmt"
	"sync"
)

// Message is a text message the bot sent to a chat.
type Message struct {
	ChatID int64
	Text   string
}

// Document is a file the bot sent to a chat.
type Document struct {
	ChatID   int64
	Filename string
	Data     []byte
	Caption  string
}

// FakeSender is a thread-safe in-memory telegram.Sender that records everything the bot sends and
// serves registered files for download.
type FakeSender struct {
	mu       sync.Mutex
	files    map[string][]byte
	messages []Message
	docs     []Document
}

// NewFakeSender creates an empty FakeSender.
func NewFakeSender() *FakeSender {
	return &FakeSender{files: map[string][]byte{}}
}

// AddFile registers bytes served by DownloadPhoto and DownloadDocument under fileID.
func (f *FakeSender) AddFile(fileID string, data []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.files == nil {
		f.files = map[string][]byte{}
	}
	f.files[fileID] = append([]byte(nil), data...)
}

func (f *FakeSender) download(fileID string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	data, ok := f.files[fileID]
	if !ok {
		return nil, fmt.Errorf("telegramtest: unknown file id %q", fileID)
	}
	return append([]byte(nil), data...), nil
}

// SendMessage records a sent message.
func (f *FakeSender) SendMessage(chatID int64, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.messages = append(f.messages, Message{ChatID: chatID, Text: text})
	return nil
}

// DownloadPhoto returns the bytes registered for fileID.
func (f *FakeSender) DownloadPhoto(fileID string) ([]byte, error) { return f.download(fileID) }

// DownloadDocument returns the bytes registered for fileID.
func (f *FakeSender) DownloadDocument(fileID string) ([]byte, error) { return f.download(fileID) }

// SendDocument records a sent document.
func (f *FakeSender) SendDocument(chatID int64, filename string, data []byte, caption string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.docs = append(f.docs, Document{ChatID: chatID, Filename: filename, Data: append([]byte(nil), data...), Caption: caption})
	return nil
}

// Messages returns the texts sent to chatID, in order.
func (f *FakeSender) Messages(chatID int64) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, m := range f.messages {
		if m.ChatID == chatID {
			out = append(out, m.Text)
		}
	}
	return out
}

// Documents returns the documents sent to chatID, in order.
func (f *FakeSender) Documents(chatID int64) []Document {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []Document
	for _, d := range f.docs {
		if d.ChatID == chatID {
			out = append(out, d)
		}
	}
	return out
}
