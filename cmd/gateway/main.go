package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	tt "github.com/httpjamesm/matrix-tiktok/pkg/libtiktok"
	"github.com/rs/zerolog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	gw "tiktok-chat-bot/internal/gateway"
	"time"
)

func env(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}
func secret(name string) (string, error) {
	path := os.Getenv(name)
	if path == "" {
		return "", fmt.Errorf("%s must name a secret file", name)
	}
	b, err := os.ReadFile(path)
	if err != nil || len(b) > 65536 {
		return "", fmt.Errorf("%s cannot be read", name)
	}
	v := strings.TrimSpace(string(b))
	if v == "" || strings.ContainsAny(v, "\r\n") {
		return "", fmt.Errorf("%s must contain one nonempty line", name)
	}
	return v, nil
}
func main() {
	zerolog.SetGlobalLevel(zerolog.Disabled)
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run() error {
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		c := http.Client{Timeout: 2 * time.Second}
		r, err := c.Get("http://127.0.0.1:8080/healthz")
		if err != nil {
			return errors.New("unhealthy")
		}
		r.Body.Close()
		if r.StatusCode != 200 {
			return errors.New("unhealthy")
		}
		return nil
	}
	account := os.Getenv("TIKTOK_ACCOUNT_ID")
	id, err := strconv.ParseUint(account, 10, 64)
	if err != nil || id == 0 || strconv.FormatUint(id, 10) != account {
		return errors.New("TIKTOK_ACCOUNT_ID must be a numeric string")
	}
	dir := env("DATA_DIR", "/data")
	s, err := gw.Open(filepath.Join(dir, "gateway.db"), account)
	if err != nil {
		return errors.New("cannot open database: locked, account mismatch, or storage error")
	}
	defer s.Close()
	if len(os.Args) > 1 && os.Args[1] == "import" {
		flags := flag.NewFlagSet("import", flag.ContinueOnError)
		source := flags.String("legacy-state", "", "read-only legacy state.json")
		if err = flags.Parse(os.Args[2:]); err != nil {
			return err
		}
		if *source == "" {
			return errors.New("import requires --legacy-state")
		}
		b, err := os.ReadFile(*source)
		if err != nil {
			return errors.New("cannot read legacy state")
		}
		if err = s.Import(b); err != nil {
			return err
		}
		fmt.Println("legacy_import_complete_paused")
		return nil
	}
	if len(os.Args) > 1 {
		return errors.New("usage: gateway [import --legacy-state PATH | healthcheck]")
	}
	token, err := secret("API_TOKEN_FILE")
	if err != nil {
		return err
	}
	webhookURL := os.Getenv("WEBHOOK_URL")
	webhookToken := ""
	if webhookURL != "" {
		u, e := url.Parse(webhookURL)
		if e != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.Fragment != "" {
			return errors.New("invalid WEBHOOK_URL")
		}
		webhookToken, err = secret("WEBHOOK_TOKEN_FILE")
		if err != nil {
			return err
		}
		if token == webhookToken {
			return errors.New("API and webhook tokens must differ")
		}
	}
	var api gw.API
	if os.Getenv("TIKTOK_MODE") == "mock" {
		api = &gw.Mock{EventsFile: os.Getenv("MOCK_EVENTS_FILE"), MessagesFile: os.Getenv("MOCK_MESSAGES_FILE")}
	} else if mode := os.Getenv("TIKTOK_MODE"); mode == "" || mode == "live" {
		cookie, e := secret("TIKTOK_COOKIE_FILE")
		if e != nil {
			return e
		}
		source := os.Getenv("STICKER_SOURCE_USER_ID")
		if source != "" {
			n, e := strconv.ParseUint(source, 10, 64)
			if e != nil || n == 0 || source == account {
				return errors.New("invalid STICKER_SOURCE_USER_ID")
			}
		}
		api = &gw.TikTok{Client: tt.NewClient(cookie, ""), AccountID: account, Username: os.Getenv("TIKTOK_EXPECTED_USERNAME"), StickerSource: source}
	} else {
		return errors.New("TIKTOK_MODE must be live or mock")
	}
	poll, err := strconv.Atoi(env("POLL_SECONDS", "45"))
	if err != nil || poll < 1 || poll > 3600 {
		return errors.New("invalid POLL_SECONDS")
	}
	if os.Getenv("TIKTOK_MODE") != "mock" && poll < 30 {
		return errors.New("live POLL_SECONDS must be at least 30")
	}
	receiveMessages := env("RECEIVE_MESSAGES", "false")
	if receiveMessages != "true" && receiveMessages != "false" {
		return errors.New("RECEIVE_MESSAGES must be true or false")
	}
	if err = s.Recover(); err != nil {
		return errors.New("database_recovery_failed")
	}
	e := &gw.Engine{Store: s, API: api, VerifyDelay: 2 * time.Second}
	hook := &gw.Webhook{Store: s, URL: webhookURL, Token: webhookToken}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	failures := make(chan error, 4)
	var wg sync.WaitGroup
	start := func(interval time.Duration, fn func(context.Context) error) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ticker := time.NewTicker(interval)
			defer ticker.Stop()
			for {
				if ctx.Err() != nil {
					return
				}
				if err := fn(ctx); err != nil {
					failures <- errors.New("storage_failure_stopping_service")
					return
				}
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
				}
			}
		}()
	}
	start(time.Duration(poll)*time.Second, e.Poll)
	// Opt in after configuring a receiver that supports message.received.
	if receiveMessages == "true" {
		start(time.Duration(poll)*time.Second, e.PollMessages)
	}
	start(time.Second, e.Work)
	start(time.Second, hook.Deliver)
	server := &http.Server{Addr: env("LISTEN_ADDR", ":8080"), Handler: e.Handler(token), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 180 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16384, BaseContext: func(_ net.Listener) context.Context { return ctx }}
	go func() {
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			failures <- errors.New("http_server_failed")
		}
	}()
	fmt.Println("gateway_started")
	var result error
	select {
	case <-ctx.Done():
	case result = <-failures:
	}
	stop()
	shutdown, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	server.Shutdown(shutdown)
	wg.Wait()
	fmt.Println("gateway_stopped")
	return result
}
