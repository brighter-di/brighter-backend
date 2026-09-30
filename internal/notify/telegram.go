// Package notify posts Brighter's events to Telegram: in full to an operator
// chat, and without portfolio ids or wallet addresses to every chat that
// pressed Start on the bot.
//
// Sending is asynchronous and best-effort: a slow or failing Telegram never
// delays or fails the request that raised the event. The bot token is a
// secret; it appears only in request URLs and never in a log or an error.
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Subscribers keeps the chats that pressed Start on the bot.
type Subscribers interface {
	AddSubscriber(ctx context.Context, chatID int64) (bool, error)
	RemoveSubscriber(ctx context.Context, chatID int64) error
	Subscribers(ctx context.Context) ([]int64, error)
}

// Message is one event, worded twice: Operator in full for the operator chat,
// and Public without portfolio ids or wallet addresses for every subscriber.
// An empty text is not sent.
type Message struct {
	Operator string
	Public   string
}

type Options struct {
	Token       string
	ChatID      string // the operator chat
	BaseURL     string // default https://api.telegram.org
	Subscribers Subscribers
	Logger      *slog.Logger
	HTTPClient  *http.Client
	// Pause between messages to subscribers, keeping a broadcast under
	// Telegram's limit of 30 messages a second. Default 50ms.
	SendGap time.Duration
	// How long one request for updates waits for news. Default 50s.
	PollTimeout time.Duration
}

type Notifier struct {
	token   string
	chatID  string
	base    string
	subs    Subscribers
	http    *http.Client
	log     *slog.Logger
	queue   chan Message
	gap     time.Duration
	pollFor time.Duration
}

// What the bot answers /start and /stop with.
const (
	welcome = "You're subscribed to Brighter updates: new strategies and portfolios, allocation changes, rebalances and withdrawals. Send /stop to unsubscribe."
	goodbye = "You're unsubscribed. Send /start to subscribe again."
)

func New(o Options) *Notifier {
	base := strings.TrimRight(o.BaseURL, "/")
	if base == "" {
		base = "https://api.telegram.org"
	}
	hc := o.HTTPClient
	if hc == nil {
		hc = &http.Client{}
	}
	log := o.Logger
	if log == nil {
		log = slog.Default()
	}
	n := &Notifier{
		token: o.Token, chatID: o.ChatID, base: base, subs: o.Subscribers, http: hc, log: log.With("component", "notify"),
		queue: make(chan Message, 100), gap: o.SendGap, pollFor: o.PollTimeout,
	}
	if n.gap <= 0 {
		n.gap = 50 * time.Millisecond
	}
	if n.pollFor <= 0 {
		n.pollFor = 50 * time.Second
	}
	return n
}

// Enabled reports whether a bot token and an operator chat are configured.
// Together they switch the whole bot on, subscriptions included, so a
// development copy holding only the token never competes with production for
// the bot's updates. A nil Notifier is valid and disabled.
func (n *Notifier) Enabled() bool { return n != nil && n.token != "" && n.chatID != "" }

// Send queues an event. A full queue drops it rather than block the caller.
func (n *Notifier) Send(m Message) {
	if !n.Enabled() {
		return
	}
	select {
	case n.queue <- m:
	default:
		n.log.Warn("notification dropped: queue full")
	}
}

// Run delivers queued events, one at a time, until ctx is cancelled.
func (n *Notifier) Run(ctx context.Context) {
	if !n.Enabled() {
		return
	}
	for {
		select {
		case <-ctx.Done():
			return
		case m := <-n.queue:
			n.deliver(ctx, m)
		}
	}
}

func (n *Notifier) deliver(ctx context.Context, m Message) {
	if m.Operator != "" {
		if err := n.post(ctx, n.chatID, m.Operator); err != nil {
			n.log.Warn("notification not delivered", "err", err)
		}
	}
	if m.Public != "" && n.subs != nil {
		n.broadcast(ctx, m.Public)
	}
}

// broadcast sends text to every subscriber but the operator chat, which has
// the full message. A chat that blocked the bot is unsubscribed.
func (n *Notifier) broadcast(ctx context.Context, text string) {
	ids, err := n.subs.Subscribers(ctx)
	if err != nil {
		n.log.Warn("subscribers unreadable", "err", err)
		return
	}
	for _, id := range ids {
		if strconv.FormatInt(id, 10) == n.chatID {
			continue
		}
		if !pause(ctx, n.gap) {
			return
		}
		err := n.post(ctx, id, text)
		var ae *apiError
		if errors.As(err, &ae) && ae.RetryAfter > 0 {
			// Over Telegram's rate limit: wait as told, then try once more.
			if !pause(ctx, time.Duration(ae.RetryAfter)*time.Second) {
				return
			}
			err = n.post(ctx, id, text)
		}
		switch {
		case err == nil:
		case errors.As(err, &ae) && ae.Status == http.StatusForbidden:
			// Blocked the bot or deleted the account: stop writing to it.
			if err := n.subs.RemoveSubscriber(ctx, id); err != nil {
				n.log.Warn("subscriber not removed", "chat", id, "err", err)
			}
		default:
			n.log.Warn("broadcast not delivered", "chat", id, "err", err)
		}
	}
}

// Poll reads the bot's updates until ctx is cancelled: /start subscribes a
// chat, and /stop or blocking the bot unsubscribes it.
func (n *Notifier) Poll(ctx context.Context) {
	if !n.Enabled() || n.subs == nil {
		return
	}
	var offset int64
	for ctx.Err() == nil {
		updates, err := n.updates(ctx, offset)
		if err != nil {
			if ctx.Err() == nil {
				n.log.Warn("bot updates unreadable", "err", err)
				pause(ctx, 5*time.Second)
			}
			continue
		}
		for _, u := range updates {
			offset = u.UpdateID + 1
			n.handle(ctx, u)
		}
	}
}

type chat struct {
	ID int64 `json:"id"`
}

// update is the part of a Bot API update the bot reads.
type update struct {
	UpdateID int64 `json:"update_id"`
	Message  *struct {
		Chat chat   `json:"chat"`
		Text string `json:"text"`
	} `json:"message"`
	MyChatMember *struct {
		Chat          chat `json:"chat"`
		NewChatMember struct {
			Status string `json:"status"`
		} `json:"new_chat_member"`
	} `json:"my_chat_member"`
}

// updates long-polls for updates from offset on.
func (n *Notifier) updates(ctx context.Context, offset int64) ([]update, error) {
	ctx, cancel := context.WithTimeout(ctx, n.pollFor+10*time.Second)
	defer cancel()
	var out []update
	err := n.call(ctx, "getUpdates", map[string]any{
		"offset":          offset,
		"timeout":         int(n.pollFor / time.Second),
		"allowed_updates": []string{"message", "my_chat_member"},
	}, &out)
	return out, err
}

func (n *Notifier) handle(ctx context.Context, u update) {
	switch {
	case u.Message != nil:
		id := u.Message.Chat.ID
		switch command(u.Message.Text) {
		case "/start":
			if _, err := n.subs.AddSubscriber(ctx, id); err != nil {
				n.log.Warn("subscriber not recorded", "chat", id, "err", err)
				return
			}
			n.log.Info("subscribed", "chat", id)
			n.reply(ctx, id, welcome)
		case "/stop":
			if err := n.subs.RemoveSubscriber(ctx, id); err != nil {
				n.log.Warn("subscriber not removed", "chat", id, "err", err)
				return
			}
			n.log.Info("unsubscribed", "chat", id)
			n.reply(ctx, id, goodbye)
		}
	case u.MyChatMember != nil:
		if s := u.MyChatMember.NewChatMember.Status; s == "kicked" || s == "left" {
			if err := n.subs.RemoveSubscriber(ctx, u.MyChatMember.Chat.ID); err != nil {
				n.log.Warn("subscriber not removed", "chat", u.MyChatMember.Chat.ID, "err", err)
			}
		}
	}
}

// command is a message's bot command without arguments or a bot mention:
// "/start@brighterFinanceBot ref" → "/start".
func command(text string) string {
	f := strings.Fields(text)
	if len(f) == 0 {
		return ""
	}
	c, _, _ := strings.Cut(f[0], "@")
	return strings.ToLower(c)
}

func (n *Notifier) reply(ctx context.Context, chat int64, text string) {
	if err := n.post(ctx, chat, text); err != nil {
		n.log.Warn("reply not delivered", "chat", chat, "err", err)
	}
}

func (n *Notifier) post(ctx context.Context, chat any, text string) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return n.call(ctx, "sendMessage", map[string]any{"chat_id": chat, "text": text, "disable_web_page_preview": true}, nil)
}

// apiError is a request Telegram refused. It never carries the token.
type apiError struct {
	Status      int
	Description string
	// Seconds to wait before retrying, when rate-limited.
	RetryAfter int
}

func (e *apiError) Error() string { return fmt.Sprintf("telegram %d: %s", e.Status, e.Description) }

// call invokes a Bot API method and decodes its result into result, if given.
// Errors never carry the token, which is part of the URL.
func (n *Notifier) call(ctx context.Context, method string, body, result any) error {
	payload, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.base+"/bot"+n.token+"/"+method, bytes.NewReader(payload))
	if err != nil {
		return errors.New("telegram: invalid request")
	}
	req.Header.Set("content-type", "application/json")
	res, err := n.http.Do(req)
	if err != nil {
		// A transport error quotes the URL, and the URL carries the token.
		var ue *url.Error
		if errors.As(err, &ue) {
			return fmt.Errorf("telegram: %w", ue.Err)
		}
		return errors.New("telegram: request failed")
	}
	defer res.Body.Close()
	var out struct {
		Result      json.RawMessage `json:"result"`
		Description string          `json:"description"`
		Parameters  struct {
			RetryAfter int `json:"retry_after"`
		} `json:"parameters"`
	}
	decodeErr := json.NewDecoder(io.LimitReader(res.Body, 4<<20)).Decode(&out)
	if res.StatusCode != http.StatusOK {
		return &apiError{Status: res.StatusCode, Description: out.Description, RetryAfter: out.Parameters.RetryAfter}
	}
	if decodeErr != nil {
		return fmt.Errorf("telegram %s: unreadable response", method)
	}
	if result != nil && len(out.Result) > 0 {
		if err := json.Unmarshal(out.Result, result); err != nil {
			return fmt.Errorf("telegram %s: unreadable result", method)
		}
	}
	return nil
}

// pause waits d, or less if ctx ends first; false when it did.
func pause(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
