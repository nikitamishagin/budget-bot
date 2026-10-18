package telegram

import (
	"context"
	"errors"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// failingSender fails every download.
type failingSender struct {
	fakeSender
}

func (f *failingSender) DownloadPhoto(string) ([]byte, error)    { return nil, errors.New("boom") }
func (f *failingSender) DownloadDocument(string) ([]byte, error) { return nil, errors.New("boom") }

var _ = Describe("HandleUpdate", func() {
	var (
		sender *fakeSender
		bot    *Bot
		ctx    context.Context
	)

	BeforeEach(func() {
		ctx = context.Background()
		sender = &fakeSender{photo: []byte("img")}
		bot = newTestBot(sender, &fakeParser{}, &fakeEnricher{}, newFakeAccounts(), []string{"alice"})
		sender.messages = nil
	})

	It("ignores users that are not allowed", func() {
		bot.HandleUpdate(ctx, Update{ChatID: 1, Username: "mallory", Command: "start"})
		Expect(sender.messages).To(BeEmpty())
	})

	It("dispatches commands", func() {
		bot.HandleUpdate(ctx, Update{ChatID: 1, Username: "alice", Command: "start"})
		Expect(sender.messages).To(HaveLen(1))
	})

	It("downloads and handles documents", func() {
		sender.document = []byte("not sqlite")
		bot.HandleUpdate(ctx, Update{ChatID: 2, Username: "alice", Document: &File{ID: "d1", Name: "x.sqlite"}})
		Expect(sender.messages).NotTo(BeEmpty())
	})

	It("passes photo MIME through and defaults it", func() {
		bot.HandleUpdate(ctx, Update{ChatID: 1, Username: "alice", Photo: &File{ID: "p1", MIME: "image/png"}})
		bot.HandleUpdate(ctx, Update{ChatID: 1, Username: "alice", Photo: &File{ID: "p2"}})
		items := bot.screenshots.take(1)
		Expect(items).To(HaveLen(2))
		Expect(items[0].mime).To(Equal("image/png"))
		Expect(items[1].mime).To(Equal("image/jpeg"))
	})

	It("dispatches text", func() {
		bot.HandleUpdate(ctx, Update{ChatID: 1, Username: "alice", Text: "hello"})
		Expect(sender.messages).To(HaveLen(1))
	})

	It("reports download failures", func() {
		fs := &failingSender{}
		bot.Sender = fs
		bot.HandleUpdate(ctx, Update{ChatID: 1, Username: "alice", Document: &File{ID: "d"}})
		bot.HandleUpdate(ctx, Update{ChatID: 1, Username: "alice", Photo: &File{ID: "p"}})
		Expect(fs.messages).To(Equal([]string{
			"Sorry, I could not download that file.",
			"Sorry, I could not download that photo.",
		}))
	})

	It("serves updates until the channel is closed", func() {
		ch := make(chan Update, 2)
		ch <- Update{ChatID: 1, Username: "alice", Photo: &File{ID: "p"}}
		ch <- Update{ChatID: 1, Username: "mallory", Photo: &File{ID: "p"}}
		close(ch)
		bot.Serve(ctx, ch)
		Expect(bot.screenshots.take(1)).To(HaveLen(1))
	})

	It("stops serving when the context is canceled", func() {
		cctx, cancel := context.WithCancel(ctx)
		cancel()
		bot.Serve(cctx, make(chan Update))
	})
})

var _ = Describe("fromTG", func() {
	It("ignores updates without a message", func() {
		_, ok := fromTG(tgbotapi.Update{})
		Expect(ok).To(BeFalse())
	})

	It("converts commands", func() {
		u, ok := fromTG(tgbotapi.Update{Message: &tgbotapi.Message{
			Chat: &tgbotapi.Chat{ID: 7}, From: &tgbotapi.User{UserName: "alice"}, Text: "/parse",
			Entities: []tgbotapi.MessageEntity{{Type: "bot_command", Offset: 0, Length: 6}},
		}})
		Expect(ok).To(BeTrue())
		Expect(u.ChatID).To(Equal(int64(7)))
		Expect(u.Username).To(Equal("alice"))
		Expect(u.Command).To(Equal("parse"))
	})

	It("picks the largest photo and documents", func() {
		u, _ := fromTG(tgbotapi.Update{Message: &tgbotapi.Message{
			Chat:  &tgbotapi.Chat{ID: 1},
			Photo: []tgbotapi.PhotoSize{{FileID: "small"}, {FileID: "big"}},
		}})
		Expect(u.Photo).To(Equal(&File{ID: "big", MIME: "image/jpeg"}))

		u, _ = fromTG(tgbotapi.Update{Message: &tgbotapi.Message{
			Chat:     &tgbotapi.Chat{ID: 1},
			Document: &tgbotapi.Document{FileID: "d", FileName: "a.sqlite"},
		}})
		Expect(u.Document.ID).To(Equal("d"))
		Expect(u.Document.Name).To(Equal("a.sqlite"))
	})
})
