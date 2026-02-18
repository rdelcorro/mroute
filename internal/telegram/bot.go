package telegram

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

type Bot struct {
	token   string
	chatID  string
	enabled bool
	client  *http.Client
	msgCh   chan string
	done    chan struct{}
}

func NewBot(token, chatID string) *Bot {
	enabled := token != "" && chatID != ""
	if enabled {
		log.Printf("Telegram bot enabled (chat: %s)", chatID)
	} else {
		log.Println("Telegram bot disabled (set TELEGRAM_BOT_TOKEN + TELEGRAM_CHAT_ID)")
	}
	b := &Bot{
		token:   token,
		chatID:  chatID,
		enabled: enabled,
		client:  &http.Client{Timeout: 10 * time.Second},
		msgCh:   make(chan string, 100),
		done:    make(chan struct{}),
	}
	if enabled {
		go b.sender()
	}
	return b
}

// sender processes messages from the channel with rate limiting (1/sec)
func (b *Bot) sender() {
	defer close(b.done)
	var lastSend time.Time
	for msg := range b.msgCh {
		elapsed := time.Since(lastSend)
		if elapsed < time.Second {
			time.Sleep(time.Second - elapsed)
		}
		if err := b.doSend(msg); err != nil {
			log.Printf("telegram send error: %v", err)
		}
		lastSend = time.Now()
	}
}

// Close drains the message queue and stops the sender goroutine
func (b *Bot) Close() {
	if !b.enabled {
		return
	}
	close(b.msgCh)
	// Wait for sender to finish draining
	select {
	case <-b.done:
	case <-time.After(5 * time.Second):
		log.Println("telegram: close timed out")
	}
}

func (b *Bot) Send(msg string) {
	if !b.enabled {
		return
	}
	select {
	case b.msgCh <- msg:
	default:
		log.Println("telegram: message queue full, dropping")
	}
}

func (b *Bot) doSend(msg string) error {
	url := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", b.token)
	body, _ := json.Marshal(map[string]interface{}{
		"chat_id": b.chatID, "text": msg, "parse_mode": "HTML",
	})
	resp, err := b.client.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		rb, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("telegram %d: %s", resp.StatusCode, string(rb))
	}
	return nil
}

func (b *Bot) NotifyFlowEvent(flowID, eventType, message, severity string) {
	icon := "i"
	switch severity {
	case "warning":
		icon = "!"
	case "error":
		icon = "X"
	}
	if strings.Contains(eventType, "started") {
		icon = ">"
	} else if strings.Contains(eventType, "stopped") {
		icon = "#"
	} else if strings.Contains(eventType, "failover") {
		icon = "~"
	}
	b.Send(fmt.Sprintf("[%s] <b>%s</b>\nFlow: <code>%s</code>\n%s",
		icon, esc(eventType), esc(flowID), esc(message)))
}

func (b *Bot) NotifyStartup(addr string) {
	b.Send(fmt.Sprintf("[+] <b>mroute started</b>\nListening: %s\n%s", esc(addr), time.Now().Format("15:04:05")))
}

func (b *Bot) NotifyShutdown() {
	b.Send(fmt.Sprintf("[-] <b>mroute stopped</b>\n%s", time.Now().Format("15:04:05")))
}

func esc(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	return s
}
