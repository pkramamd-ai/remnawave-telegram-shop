package handler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
)

// telegramSendDelay is roughly the inverse of the documented per-bot rate
// limit (~30 messages/second across distinct chats). We leave plenty of
// headroom so bursts don't trigger 429s mid-broadcast.
const telegramSendDelay = 50 * time.Millisecond

// BroadcastCommandHandler implements `/broadcast` for the bot admin.
//
// Two usage modes are supported:
//
//  1. Plain text inline:        "/broadcast <html-formatted text>"
//     The text after the command is sent as a regular HTML message to every
//     customer.
//
//  2. Reply-to-message:         reply with "/broadcast" to any message you
//     previously sent in the chat with the bot.
//     The replied-to message is forwarded to every customer using
//     copyMessage, preserving media, captions and formatting.
//
// The handler must be registered with isAdminMiddleware; it does not perform
// its own authorization check.
func (h Handler) BroadcastCommandHandler(ctx context.Context, b *bot.Bot, update *models.Update) {
	if update.Message == nil {
		return
	}
	chatID := update.Message.Chat.ID

	telegramIDs, err := h.customerRepository.FindAllTelegramIds(ctx)
	if err != nil {
		slog.Error("broadcast: failed to fetch telegram ids", "error", err)
		_, _ = b.SendMessage(ctx, &bot.SendMessageParams{
			ChatID: chatID,
			Text:   "❌ Не удалось прочитать базу пользователей. Попробуй позже.",
		})
		return
	}

	if len(telegramIDs) == 0 {
		_, _ = b.SendMessage(ctx, &bot.SendMessageParams{
			ChatID: chatID,
			Text:   "В базе пока нет ни одного пользователя — рассылать некому.",
		})
		return
	}

	send := pickBroadcastSender(b, update.Message)
	if send == nil {
		_, _ = b.SendMessage(ctx, &bot.SendMessageParams{
			ChatID:    chatID,
			Text:      broadcastUsage(),
			ParseMode: models.ParseModeHTML,
		})
		return
	}

	_, _ = b.SendMessage(ctx, &bot.SendMessageParams{
		ChatID: chatID,
		Text:   fmt.Sprintf("📣 Начинаю рассылку: %d получателей.", len(telegramIDs)),
	})

	sent, failed := 0, 0
	for _, recipientID := range telegramIDs {
		if err := send(ctx, recipientID); err != nil {
			failed++
			slog.Debug("broadcast: send failed", "telegram_id", recipientID, "error", err)
		} else {
			sent++
		}
		time.Sleep(telegramSendDelay)
	}

	_, _ = b.SendMessage(ctx, &bot.SendMessageParams{
		ChatID: chatID,
		Text: fmt.Sprintf(
			"✅ Рассылка завершена.\n\nОтправлено: <b>%d</b>\nНе доставлено: <b>%d</b>\nВсего в базе: <b>%d</b>\n\n<i>Не доставлено = пользователь заблокировал бота, удалил аккаунт или ещё не написал боту первым.</i>",
			sent, failed, len(telegramIDs),
		),
		ParseMode: models.ParseModeHTML,
	})
}

// pickBroadcastSender returns a function that sends one broadcast message to
// the given recipient ID, based on what the admin attached to /broadcast.
// Returns nil if neither a reply nor inline text was provided.
func pickBroadcastSender(b *bot.Bot, msg *models.Message) func(ctx context.Context, recipientID int64) error {
	if msg.ReplyToMessage != nil {
		source := msg.ReplyToMessage
		return func(ctx context.Context, recipientID int64) error {
			_, err := b.CopyMessage(ctx, &bot.CopyMessageParams{
				ChatID:     recipientID,
				FromChatID: source.Chat.ID,
				MessageID:  source.ID,
			})
			return err
		}
	}

	text := strings.TrimSpace(strings.TrimPrefix(msg.Text, "/broadcast"))
	text = strings.TrimSpace(strings.TrimPrefix(text, "/post"))
	if text == "" {
		return nil
	}
	return func(ctx context.Context, recipientID int64) error {
		_, err := b.SendMessage(ctx, &bot.SendMessageParams{
			ChatID:    recipientID,
			Text:      text,
			ParseMode: models.ParseModeHTML,
		})
		return err
	}
}

// broadcastUsage is the help text shown when /broadcast is called with no
// payload and no reply-to-message.
func broadcastUsage() string {
	return strings.Join([]string{
		"<b>Рассылка пользователям</b>",
		"",
		"<b>Способ 1 — простой текст</b>:",
		"<code>/broadcast Завтра обновляем серверы — возможны короткие перебои.</code>",
		"",
		"<b>Способ 2 — любое сообщение</b> (фото, видео, форматированный текст):",
		"1) Отправь сообщение в этот чат как обычно.",
		"2) Ответь (reply) на это сообщение командой <code>/broadcast</code>.",
		"Бот скопирует сообщение всем пользователям как есть.",
		"",
		"<i>В тексте можно использовать HTML-теги: &lt;b&gt;, &lt;i&gt;, &lt;a href&gt; и т.д.</i>",
	}, "\n")
}

// errBroadcastNoPayload is exported only for tests / future state-machine
// flows; the handler itself does not surface it.
var errBroadcastNoPayload = errors.New("broadcast called without text or reply target")
