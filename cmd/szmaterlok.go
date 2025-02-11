package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"time"

	"github.com/google/uuid"
	"github.com/sirupsen/logrus"

	"github.com/fenole/szmaterlok/service"
	"github.com/fenole/szmaterlok/storage"
)

func run(ctx context.Context) error {
	log := service.LoggerDefault()
	log.SetLevel(logrus.DebugLevel)

	if err := service.ConfigLoad(ctx); err != nil {
		return err
	}

	config := service.ConfigDefault()
	if err := service.ConfigRead(&config); err != nil {
		return err
	}

	tokenizerFactory := service.SessionTokenizerFactory{
		Timeout: time.Minute,
		Logger:  log,
	}

	tokenizer, err := tokenizerFactory.Tokenizer(&config)
	if err != nil {
		return err
	}

	storage, err := storage.NewSQLiteStorage(ctx, config.Database)
	if err != nil {
		return err
	}

	stateOnlineUsers := service.NewStateOnlineUsers()

	messageHandler := service.NewBridgeMessageHandler(log)
	lastMessagesBuffer := service.NewLastMessagesBuffer(config.LastMessagesBufferSize, log)

	stateEventRouter := service.NewBridgeEventRouter()
	stateEventRouter.Hook(service.BridgeMessageSent, lastMessagesBuffer)

	stateBuilder := service.StateBuilder{
		Archive: storage,
		Handler: stateEventRouter,
	}

	log.Println("Rebuilding state.")
	if err := stateBuilder.Rebuild(ctx); err != nil {
		return fmt.Errorf("failed to rebuild state: %w", err)
	}
	log.Println("State rebuilding process has succeed.")

	eventRouter := service.NewBridgeEventRouter()
	eventRouter.Hook(service.BridgeMessageSent, messageHandler)
	eventRouter.Hook(service.BridgeUserJoin, messageHandler)
	eventRouter.Hook(service.BridgeUserLeft, messageHandler)
	eventRouter.Hook(service.BridgeUserJoin, service.StateUserJoinHook(log, stateOnlineUsers))
	eventRouter.Hook(service.BridgeUserLeft, service.StateUserLeftHook(log, stateOnlineUsers))
	eventRouter.Hook(service.BridgeMessageSent, lastMessagesBuffer)

	bridge := service.NewBridge(ctx, service.BridgeBuilder{
		Handler: eventRouter,
		Logger:  log,
		Storage: storage,
	})

	clock := service.ClockFunc(time.Now)
	r := service.NewRouter(service.RouterDependencies{
		Logger: log,
		SessionStore: &service.SessionCookieStore{
			ExpirationTime: time.Hour * 24 * 7,
			Tokenizer:      tokenizer,
			Clock:          clock,
		},
		Bridge:            bridge,
		AllChatUsersStore: stateOnlineUsers,
		MessageNotifier: &service.MessageNotifierWithBuffer{
			Notifier: messageHandler,
			Buffer:   lastMessagesBuffer,
			Logger:   log,
		},
		IDGenerator: service.IDGeneratorFunc(uuid.NewString),
		Clock:       clock,
	})

	c := make(chan os.Signal, 1)
	errc := make(chan error, 1)

	wait := time.Second * 15
	srv := &http.Server{
		Addr:    config.Address,
		Handler: r,
		// TODO(thinkofher): Come back later to setup timeouts.
	}

	log.Println("Starting szmaterlok")
	// Run our server in a goroutine so that it doesn't block.
	go func() {
		log.Printf("Listening at %s", config.Address)
		if err := srv.ListenAndServe(); err != nil {
			errc <- err
		}
	}()

	// We'll accept graceful shutdowns when quit via SIGINT (Ctrl+C)
	// SIGKILL, SIGQUIT or SIGTERM (Ctrl+/) will not be caught.
	signal.Notify(c, os.Interrupt)

	// Block until we receive our signal or error from server.
	select {
	case <-c:
		ctx, cancel := context.WithTimeout(ctx, wait)
		defer cancel()
		// Doesn't block if no connections, but will otherwise wait
		// until the timeout deadline.
		srv.Shutdown(ctx)

		// Wait for bridge to process its jobs.
		bridge.Shutdown(ctx)

		// Optionally, you could run srv.Shutdown in a goroutine and block on
		// <-ctx.Done() if your application should wait for other services
		// to finalize based on context cancellation.
		log.Println("Shutting down")
		return nil
	case err := <-errc:
		return err
	}
}

func main() {
	if err := run(context.Background()); err != nil {
		log.Fatal("szmaterlok:", err.Error())
	}
}
