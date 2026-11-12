package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

var Version = "v0.2.0"

// loadDotEnv intentionally supports single-line KEY=value, quoted strings and
// comment lines only. No shell evaluation or variable interpolation is performed.
func loadDotEnv(path string) error {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 4096), 1<<20)
	line := 0
	for scanner.Scan() {
		line++
		s := strings.TrimSpace(scanner.Text())
		if s == "" || strings.HasPrefix(s, "#") {
			continue
		}
		s = strings.TrimPrefix(s, "export ")
		key, value, ok := strings.Cut(s, "=")
		key = strings.TrimSpace(key)
		if !ok || key == "" || strings.IndexFunc(key, func(r rune) bool {
			return !(r == '_' || r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9')
		}) >= 0 {
			return fmt.Errorf("invalid .env entry at line %d", line)
		}
		value = strings.TrimSpace(value)
		if strings.HasPrefix(value, "\"") || strings.HasPrefix(value, "'") {
			quote := value[0]
			if len(value) < 2 || value[len(value)-1] != quote {
				return fmt.Errorf("unclosed .env quote at line %d", line)
			}
			value = value[1 : len(value)-1]
		}
		if _, exists := os.LookupEnv(key); !exists {
			if err := os.Setenv(key, value); err != nil {
				return fmt.Errorf("invalid .env entry at line %d", line)
			}
		}
	}
	return scanner.Err()
}

func envDuration(key string, def int64, unit time.Duration) (time.Duration, error) {
	raw := os.Getenv(key)
	if raw == "" {
		return time.Duration(def) * unit, nil
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n <= 0 || n > int64((1<<63-1)/unit) {
		return 0, fmt.Errorf("%s must be a positive duration", key)
	}
	return time.Duration(n) * unit, nil
}
func configFromEnv() (Config, error) {
	c := Config{BaseURL: os.Getenv("OPENWEBUI_URL"), APIKey: os.Getenv("PROXY_API_KEY"), DefaultModel: os.Getenv("DEFAULT_MODEL"), OutboundProxy: os.Getenv("OUTBOUND_PROXY")}
	var err error
	c.Timeout, err = envDuration("UPSTREAM_TIMEOUT_MS", 300000, time.Millisecond)
	if err != nil {
		return c, err
	}
	c.Cooldown, err = envDuration("ACCOUNT_COOLDOWN_SECONDS", 60, time.Second)
	if err != nil {
		return c, err
	}
	configPath := os.Getenv("CONFIG_FILE")
	if configPath == "" && os.Getenv("ACCOUNTS_FILE") == "" && os.Getenv("OPENWEBUI_EMAIL") == "" && os.Getenv("OPENWEBUI_PASSWORD") == "" {
		configPath = "data/config.json"
	}
	if path := configPath; path != "" {
		c.ConfigPath = path
		f, err := os.Open(path)
		if errors.Is(err, os.ErrNotExist) {
			c.Websites = []WebsiteConfig{}
			c.Accounts = []AccountConfig{}
			return c, nil
		}
		if err != nil {
			return c, errors.New("could not open CONFIG_FILE")
		}
		defer f.Close()
		raw, err := io.ReadAll(io.LimitReader(f, (4<<20)+1))
		if err != nil || len(raw) > 4<<20 {
			return c, errors.New("CONFIG_FILE must be readable and at most 4 MiB")
		}
		if len(strings.TrimSpace(string(raw))) == 0 || strings.TrimSpace(string(raw))[0] != '{' {
			return c, errors.New("CONFIG_FILE must be a JSON object with websites and accounts")
		}
		var doc configDocument
		decoder := json.NewDecoder(strings.NewReader(string(raw)))
		decoder.DisallowUnknownFields()
		if decoder.Decode(&doc) != nil {
			return c, errors.New("CONFIG_FILE must contain supported websites and accounts fields")
		}
		var extra any
		if decoder.Decode(&extra) != io.EOF {
			return c, errors.New("CONFIG_FILE contains trailing data")
		}
		c.Websites = doc.Websites
		c.Accounts = doc.Accounts
		return c, nil
	}
	if path := os.Getenv("ACCOUNTS_FILE"); path != "" {
		f, err := os.Open(path)
		if err != nil {
			return c, errors.New("cannot open ACCOUNTS_FILE")
		}
		defer f.Close()
		raw, err := io.ReadAll(io.LimitReader(f, (4<<20)+1))
		if err != nil || len(raw) > 4<<20 {
			return c, errors.New("ACCOUNTS_FILE must be readable and at most 4 MiB")
		}
		decoder := json.NewDecoder(strings.NewReader(string(raw)))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&c.Accounts); err != nil {
			return c, errors.New("ACCOUNTS_FILE must be an array of account objects with supported fields")
		}
		var extra any
		if decoder.Decode(&extra) != io.EOF {
			return c, errors.New("ACCOUNTS_FILE contains trailing data")
		}
	} else {
		c.Accounts = []AccountConfig{{ID: "default", Email: os.Getenv("OPENWEBUI_EMAIL"), Password: os.Getenv("OPENWEBUI_PASSWORD")}}
	}
	return c, nil
}

func run() error {
	if err := loadDotEnv(".env"); err != nil {
		return err
	}
	args := os.Args[1:]
	if len(args) > 0 {
		switch args[0] {
		case "version", "--version", "-version":
			fmt.Println(Version)
			return nil
		case "ota":
			return runOTA(args[1:])
		case "serve":
			if len(args) > 1 {
				return errors.New("usage: openwebui-proxy [serve|version|ota]")
			}
		case "help", "--help", "-h":
			fmt.Println("Usage: openwebui-proxy [serve|version|ota check|ota update|ota rollback|ota keygen|ota sign]\nServer configuration is read from environment and .env. See README.md.")
			return nil
		default:
			return errors.New("unknown command; use --help")
		}
	}
	cfg, err := configFromEnv()
	if err != nil {
		return err
	}
	app, err := NewApplication(cfg, os.Getenv("ADMIN_API_KEY"))
	if err != nil {
		return err
	}
	host := os.Getenv("HOST")
	if host == "" {
		host = "127.0.0.1"
	}
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	portNumber, err := strconv.Atoi(port)
	if err != nil || portNumber < 1 || portNumber > 65535 {
		return errors.New("PORT must be between 1 and 65535")
	}
	listener, err := net.Listen("tcp", net.JoinHostPort(host, port))
	if err != nil {
		return err
	}
	defer app.CloseIdleConnections()
	server := &http.Server{Handler: app, ReadHeaderTimeout: 15 * time.Second, ReadTimeout: 60 * time.Second, IdleTimeout: 90 * time.Second, MaxHeaderBytes: 64 << 10}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	log.Printf("Open WebUI proxy %s listening on %s (%d accounts)", Version, listener.Addr(), len(cfg.Accounts))
	select {
	case err := <-done:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		err := server.Shutdown(shutdownCtx)
		if err != nil {
			_ = server.Close()
		}
		return nil
	}
}
func main() {
	if err := run(); err != nil {
		log.Printf("Error: %s", err)
		os.Exit(1)
	}
}
