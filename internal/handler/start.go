package handler

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
	"log/slog"

	"remnawave-tg-shop-bot/internal/config"
	"remnawave-tg-shop-bot/internal/database"
	"remnawave-tg-shop-bot/internal/remnawave"
	"remnawave-tg-shop-bot/utils"
)

func (h Handler) StartCommandHandler(ctx context.Context, b *bot.Bot, update *models.Update) {
	ctxWithTime, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	langCode := update.Message.From.LanguageCode
	existingCustomer, err := h.customerRepository.FindByTelegramId(ctx, update.Message.Chat.ID)
	if err != nil {
		slog.Error("error finding customer by telegram id", "error", err)
		return
	}

	if existingCustomer == nil {
		existingCustomer, err = h.customerRepository.Create(ctxWithTime, &database.Customer{
			TelegramID: update.Message.Chat.ID,
			Language:   langCode,
		})
		if err != nil {
			slog.Error("error creating customer", "error", err)
			return
		}

		if strings.Contains(update.Message.Text, "ref_") {
			arg := strings.Split(update.Message.Text, " ")[1]
			if strings.HasPrefix(arg, "ref_") {
				code := strings.TrimPrefix(arg, "ref_")
				referrerId, err := strconv.ParseInt(code, 10, 64)
				if err != nil {
					slog.Error("error parsing referrer id", "error", err)
					return
				}
				_, err = h.customerRepository.FindByTelegramId(ctx, referrerId)
				if err == nil {
					_, err := h.referralRepository.Create(ctx, referrerId, existingCustomer.TelegramID)
					if err != nil {
						slog.Error("error creating referral", "error", err)
						return
					}
					slog.Info("referral created", "referrerId", utils.MaskHalfInt64(referrerId), "refereeId", utils.MaskHalfInt64(existingCustomer.TelegramID))
				}
			}
		}
	} else {
		updates := map[string]interface{}{
			"language": langCode,
		}

		err = h.customerRepository.UpdateFields(ctx, existingCustomer.ID, updates)
		if err != nil {
			slog.Error("Error updating customer", "error", err)
			return
		}
	}

	// Refresh the per-chat menu button on every /start. Telegram only applies
	// the bot's *global* default menu button to chats that have not seen any
	// per-chat configuration; clients that opened the chat before the global
	// default existed will keep showing the old "commands" button. Setting
	// the per-chat button here guarantees every active user sees the Mini
	// App entry point, without requiring them to clear cache or re-add the
	// bot.
	if mini := config.GetMiniAppURL(); mini != "" {
		_, err := b.SetChatMenuButton(ctx, &bot.SetChatMenuButtonParams{
			ChatID: update.Message.Chat.ID,
			MenuButton: models.MenuButtonWebApp{
				Type:   models.MenuButtonTypeWebApp,
				Text:   "VPN",
				WebApp: models.WebAppInfo{URL: mini},
			},
		})
		if err != nil {
			slog.Warn("could not set per-chat menu button", "error", err)
		}
	}

	inlineKeyboard := h.buildStartKeyboard(existingCustomer, langCode)

	m, err := b.SendMessage(ctx, &bot.SendMessageParams{
		ChatID: update.Message.Chat.ID,
		Text:   "🧹",
		ReplyMarkup: models.ReplyKeyboardRemove{
			RemoveKeyboard: true,
		},
	})

	if err != nil {
		slog.Error("Error sending removing reply keyboard", "error", err)
		return
	}

	_, err = b.DeleteMessage(ctx, &bot.DeleteMessageParams{
		ChatID:    update.Message.Chat.ID,
		MessageID: m.ID,
	})

	if err != nil {
		slog.Error("Error deleting message", "error", err)
		return
	}

	_, err = b.SendMessage(ctx, &bot.SendMessageParams{
		ChatID:    update.Message.Chat.ID,
		ParseMode: models.ParseModeHTML,
		ReplyMarkup: models.InlineKeyboardMarkup{
			InlineKeyboard: inlineKeyboard,
		},
		Text: h.translation.GetText(langCode, "greeting"),
	})
	if err != nil {
		slog.Error("Error sending /start message", "error", err)
	}

	// Deep-link: /start buy_<n>[_<method>] jumps straight to payment.
	//   buy_<n>             -> show payment-method buttons
	//   buy_<n>_cryptobot   -> directly create a CryptoBot invoice and post link
	//   buy_<n>_yookassa    -> directly create a YooKassa invoice and post link
	//   buy_<n>_stars       -> show payment-method buttons (Stars are best paid via
	//                          the bot's existing flow that uses sendInvoice)
	if arg := startPayload(update.Message.Text); strings.HasPrefix(arg, "buy_") {
		rest := strings.TrimPrefix(arg, "buy_")
		parts := strings.SplitN(rest, "_", 2)
		monthStr := parts[0]
		method := ""
		if len(parts) > 1 {
			method = parts[1]
		}
		month, err := strconv.Atoi(monthStr)
		if err != nil || config.Price(month) <= 0 {
			return
		}

		invoiceType := mapDeepLinkMethod(method)
		if invoiceType == database.InvoiceTypeCrypto || invoiceType == database.InvoiceTypeYookasa {
			h.handleDirectInvoice(ctx, b, update.Message.Chat.ID, langCode, existingCustomer, month, invoiceType)
			return
		}

		amount := strconv.Itoa(config.Price(month))
		keyboard := h.BuildSellKeyboard(ctx, update.Message.Chat.ID, langCode, monthStr, amount)
		_, sendErr := b.SendMessage(ctx, &bot.SendMessageParams{
			ChatID:    update.Message.Chat.ID,
			ParseMode: models.ParseModeHTML,
			Text:      h.translation.GetText(langCode, "pricing_info"),
			ReplyMarkup: models.InlineKeyboardMarkup{
				InlineKeyboard: keyboard,
			},
		})
		if sendErr != nil {
			slog.Error("Error sending buy deep-link message", "error", sendErr)
		}
	}
}

// mapDeepLinkMethod maps the suffix of `buy_<n>_<method>` deep links to the
// internal InvoiceType used by the payment service. Returns empty string for
// unknown values so callers fall through to the payment-methods picker.
func mapDeepLinkMethod(method string) database.InvoiceType {
	switch strings.ToLower(method) {
	case "cryptobot", "crypto":
		return database.InvoiceTypeCrypto
	case "yookassa", "yookasa", "card":
		return database.InvoiceTypeYookasa
	case "stars", "telegram":
		return database.InvoiceTypeTelegram
	default:
		return ""
	}
}

// handleDirectInvoice creates a payment for the given method and sends the
// user a message with the "Pay" button. Used when the mini-app sends the user
// here via a t.me/<bot>?start=buy_<n>_<method> deep link, so the user skips
// the payment-method picker entirely.
func (h Handler) handleDirectInvoice(
	ctx context.Context,
	b *bot.Bot,
	chatID int64,
	langCode string,
	customer *database.Customer,
	month int,
	invoiceType database.InvoiceType,
) {
	price := config.Price(month)
	ctxPay, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ctxWithUsername := context.WithValue(ctxPay, remnawave.CtxKeyUsername, "")
	paymentURL, _, err := h.paymentService.CreatePurchase(ctxWithUsername, float64(price), month, customer, invoiceType)
	if err != nil {
		slog.Error("Error creating direct deep-link payment", "error", err, "method", invoiceType)
		_, _ = b.SendMessage(ctx, &bot.SendMessageParams{
			ChatID: chatID,
			Text:   h.translation.GetText(langCode, "pricing_info"),
		})
		return
	}
	monthsLabel := fmt.Sprintf("%d", month)
	_, err = b.SendMessage(ctx, &bot.SendMessageParams{
		ChatID:    chatID,
		ParseMode: models.ParseModeHTML,
		Text:      h.translation.GetText(langCode, "pricing_info"),
		ReplyMarkup: models.InlineKeyboardMarkup{
			InlineKeyboard: [][]models.InlineKeyboardButton{
				{
					h.translation.GetButton(langCode, "pay_button").InlineURL(paymentURL),
					h.translation.GetButton(langCode, "back_button").InlineCallback(fmt.Sprintf("%s?month=%s&amount=%d", CallbackSell, monthsLabel, price)),
				},
			},
		},
	})
	if err != nil {
		slog.Error("Error sending direct deep-link payment message", "error", err)
	}
}

// startPayload extracts the parameter passed via t.me/<bot>?start=<payload>.
// Returns "" when the user typed a plain /start with no arguments.
func startPayload(text string) string {
	parts := strings.SplitN(text, " ", 2)
	if len(parts) < 2 {
		return ""
	}
	return parts[1]
}

func (h Handler) StartCallbackHandler(ctx context.Context, b *bot.Bot, update *models.Update) {
	ctxWithTime, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	callback := update.CallbackQuery
	langCode := callback.From.LanguageCode

	existingCustomer, err := h.customerRepository.FindByTelegramId(ctxWithTime, callback.From.ID)
	if err != nil {
		slog.Error("error finding customer by telegram id", "error", err)
		return
	}

	inlineKeyboard := h.buildStartKeyboard(existingCustomer, langCode)

	_, err = b.EditMessageText(ctxWithTime, &bot.EditMessageTextParams{
		ChatID:    callback.Message.Message.Chat.ID,
		MessageID: callback.Message.Message.ID,
		ParseMode: models.ParseModeHTML,
		ReplyMarkup: models.InlineKeyboardMarkup{
			InlineKeyboard: inlineKeyboard,
		},
		Text: h.translation.GetText(langCode, "greeting"),
	})
	if err != nil {
		slog.Error("Error sending /start message", "error", err)
	}
}

func (h Handler) resolveConnectButton(lang string) []models.InlineKeyboardButton {
	bd := h.translation.GetButton(lang, "connect_button")

	if config.GetMiniAppURL() != "" {
		return []models.InlineKeyboardButton{bd.InlineWebApp(config.GetMiniAppURL())}
	}
	return []models.InlineKeyboardButton{bd.InlineCallback(CallbackConnect)}
}

func (h Handler) buildStartKeyboard(existingCustomer *database.Customer, langCode string) [][]models.InlineKeyboardButton {
	var inlineKeyboard [][]models.InlineKeyboardButton

	if existingCustomer.SubscriptionLink == nil && config.TrialDays() > 0 {
		inlineKeyboard = append(inlineKeyboard, []models.InlineKeyboardButton{h.translation.GetButton(langCode, "trial_button").InlineCallback(CallbackTrial)})
	}

	inlineKeyboard = append(inlineKeyboard, [][]models.InlineKeyboardButton{{h.translation.GetButton(langCode, "buy_button").InlineCallback(CallbackBuy)}}...)

	if existingCustomer.SubscriptionLink != nil && existingCustomer.ExpireAt.After(time.Now()) {
		inlineKeyboard = append(inlineKeyboard, h.resolveConnectButton(langCode))
	}

	if config.GetReferralDays() > 0 {
		inlineKeyboard = append(inlineKeyboard, []models.InlineKeyboardButton{h.translation.GetButton(langCode, "referral_button").InlineCallback(CallbackReferral)})
	}

	if config.ServerStatusURL() != "" {
		inlineKeyboard = append(inlineKeyboard, []models.InlineKeyboardButton{h.translation.GetButton(langCode, "server_status_button").InlineURL(config.ServerStatusURL())})
	}

	if config.SupportURL() != "" {
		inlineKeyboard = append(inlineKeyboard, []models.InlineKeyboardButton{h.translation.GetButton(langCode, "support_button").InlineURL(config.SupportURL())})
	}

	if config.FeedbackURL() != "" {
		inlineKeyboard = append(inlineKeyboard, []models.InlineKeyboardButton{h.translation.GetButton(langCode, "feedback_button").InlineURL(config.FeedbackURL())})
	}

	if config.ChannelURL() != "" {
		inlineKeyboard = append(inlineKeyboard, []models.InlineKeyboardButton{h.translation.GetButton(langCode, "channel_button").InlineURL(config.ChannelURL())})
	}

	if config.TosURL() != "" {
		inlineKeyboard = append(inlineKeyboard, []models.InlineKeyboardButton{h.translation.GetButton(langCode, "tos_button").InlineURL(config.TosURL())})
	}
	return inlineKeyboard
}
