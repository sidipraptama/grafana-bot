package main

import (
	"context"
	"log"
	"os/signal"
	"syscall"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"whatsapp-bot/bot"
	"whatsapp-bot/config"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	cfg, err := config.Load(ctx)
	if err != nil {
		panic(err)
	}
	if len(cfg.TelegramToken) >= 10 {
		log.Printf("[config] telegram token loaded, first 10 chars: %s", cfg.TelegramToken[:10])
	}

	tgBot, err := tgbotapi.NewBotAPI(cfg.TelegramToken)
	if err != nil {
		panic(err)
	}
	log.Printf("[telegram] authorized as @%s", tgBot.Self.UserName)

	h := bot.NewHandler(tgBot, cfg)

	u := tgbotapi.NewUpdate(0)
	u.Timeout = 60
	updates := tgBot.GetUpdatesChan(u)

	log.Println("[telegram] listening for messages...")

	for {
		select {
		case <-ctx.Done():
			log.Println("[telegram] shutting down")
			tgBot.StopReceivingUpdates()
			return
		case update := <-updates:
			if update.Message == nil {
				continue
			}
			go h.Handle(update)
		}
	}
}
