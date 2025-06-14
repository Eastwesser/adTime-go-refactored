package run

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"s1ntez/internal/config"
	"s1ntez/internal/storage/redis"
	"syscall"
)

func Run() {
	// Initialize logger
	logger, err := zap.NewProduction()
	if err != nil {
		fmt.Printf("Failed to initialize logger: %v\n", err)
		os.Exit(1)
	}
	defer logger.Sync()

	// Handle shutdown signals
	ctx, cancel := signal.NotifyContext(
		context.Background(),
		os.Interrupt,
		syscall.SIGTERM,
	)
	defer cancel()

	// Load configuration
	cfg, err := config.Load()
	if err != nil {
		logger.Fatal("Failed to load config", zap.Error(err))
	}

	// Initialize Redis client (используем pkg/redis)
	redisStorage := redis.New(
		cfg.Redis.Addr,
		cfg.Redis.Password,
		cfg.Redis.DB,
	)
	defer redisStorage.Close()

	// Initialize PostgreSQL storage
	pgStorage, err := storage.NewPostgresStorage(ctx, *cfg, redisStorage, logger)
	if err != nil {
		logger.Fatal("Failed to init PostgreSQL storage", zap.Error(err))
	}
	defer pgStorage.Close()

	botAPI, err := tgbotapi.NewBotAPI(cfg.Telegram.Token)
	if err != nil {
		logger.Fatal("failed to create bot API", zap.Error(err))
	}

	botAPI.Debug = true // Enable debug for development

	logger.Info("Bot authorized",
		zap.String("username", botAPI.Self.UserName),
		zap.Int64("id", botAPI.Self.ID),
	)

	userDialogStateManager := state_manager.New(redisStorage)

	startCmdHandler := start.New(logger, botAPI, userDialogStateManager, pgStorage)

	commandHandlersMap := map[string]bot.CommandHandler{
		"start": startCmdHandler,
	}

	// Infrastructure
	baseRepo := repository.NewAdtimeRepo()

	// TTL proxy

	// use cases

	// controller

	// Create bot instance
	tgBot, err := bot.New(
		redisStorage,
		pgStorage,
		logger,
		cfg,
		commandHandlersMap,
		nil,
	)
	if err != nil {
		logger.Fatal("Failed to create bot", zap.Error(err))
	}

	// Start the bot
	logger.Info("Starting bot")
	if err := tgBot.Start(ctx); err != nil {
		logger.Fatal("Bot stopped with error", zap.Error(err))
	}

	logger.Info("Bot shutdown gracefully")

}
