package handler

import (
	"context"
	"strings"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
)

// AdminHelpCommandHandler implements `/help_admin`. It lists every admin-only
// command the bot supports, with short usage examples. Wire only behind
// isAdminMiddleware so non-admins can't enumerate the admin surface.
func (h Handler) AdminHelpCommandHandler(ctx context.Context, b *bot.Bot, update *models.Update) {
	if update.Message == nil {
		return
	}
	text := strings.Join([]string{
		"<b>Админ-команды</b>",
		"",
		"<b>/broadcast &lt;текст&gt;</b> — разослать текст всем пользователям бота. Поддерживает HTML.",
		"<b>/broadcast</b> (как ответ на сообщение) — скопировать любое сообщение (фото, видео, форматированный текст) всем пользователям.",
		"<b>/post …</b> — синоним <code>/broadcast</code>.",
		"<b>/sync</b> — пересинхронизировать список пользователей с Remnawave-панелью.",
		"<b>/help_admin</b> — эта подсказка.",
		"",
		"<i>Команды видны только тебе (admin), остальные пользователи получат немой ответ.</i>",
	}, "\n")

	_, _ = b.SendMessage(ctx, &bot.SendMessageParams{
		ChatID:    update.Message.Chat.ID,
		Text:      text,
		ParseMode: models.ParseModeHTML,
	})
}
