// telegram/client.go

package telegram

import (
	"errors"
	"fmt"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api"
)

type TelegramClient struct {
	bot *tgbotapi.BotAPI
}

func NewTelegramClient(token string) (*TelegramClient, error) {
	bot, err := tgbotapi.NewBotAPI(token)
	if err != nil {
		return nil, fmt.Errorf("telegram: creating bot: %w", err)
	}

	return &TelegramClient{
		bot: bot,
	}, nil
}

func (c *TelegramClient) Account() string { return c.bot.Self.UserName }

func (c *TelegramClient) SendLog(text string, channelId int64) error {
	msg := tgbotapi.NewMessage(channelId, text)
	msg.ParseMode = tgbotapi.ModeHTML
	msg.DisableWebPagePreview = true

	if _, err := c.bot.Send(msg); err != nil {
		return fmt.Errorf("telegram: sending log message: %w", err)
	}
	return nil
}

func RetryAfter(err error) (time.Duration, bool) {
	var tgErr tgbotapi.Error
	if errors.As(err, &tgErr) && tgErr.RetryAfter > 0 {
		return time.Duration(tgErr.RetryAfter) * time.Second, true
	}
	return 0, false
}
