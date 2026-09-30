package notify

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

var discard = slog.New(slog.NewTextHandler(io.Discard, nil))

// bot fakes the Bot API: sendMessage is recorded (and refused for a blocked
// chat), and getUpdates serves the pushed updates from the requested offset.
type bot struct {
	*httptest.Server
	mu      sync.Mutex
	sent    [][2]string // chat, text
	updates []map[string]any
	blocked string
}

func newBot(t *testing.T) *bot {
	b := &bot{}
	b.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		switch r.URL.Path {
		case "/botTOKEN/sendMessage":
			chat := fmt.Sprint(body["chat_id"])
			b.mu.Lock()
			blocked := chat == b.blocked
			if !blocked {
				b.sent = append(b.sent, [2]string{chat, fmt.Sprint(body["text"])})
			}
			b.mu.Unlock()
			if blocked {
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte(`{"ok":false,"description":"Forbidden: bot was blocked by the user"}`))
				return
			}
			_, _ = w.Write([]byte(`{"ok":true}`))
		case "/botTOKEN/getUpdates":
			offset, _ := body["offset"].(float64)
			b.mu.Lock()
			var out []map[string]any
			for _, u := range b.updates {
				if u["update_id"].(int) >= int(offset) {
					out = append(out, u)
				}
			}
			b.mu.Unlock()
			if len(out) == 0 {
				time.Sleep(20 * time.Millisecond)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": out})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(b.Close)
	return b
}

func (b *bot) push(u map[string]any) {
	b.mu.Lock()
	defer b.mu.Unlock()
	u["update_id"] = len(b.updates) + 1
	b.updates = append(b.updates, u)
}

// to lists the texts sent to one chat, oldest first.
func (b *bot) to(chat string) []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []string
	for _, m := range b.sent {
		if m[0] == chat {
			out = append(out, m[1])
		}
	}
	return out
}

type memSubs struct {
	mu  sync.Mutex
	ids []int64
}

func (m *memSubs) AddSubscriber(_ context.Context, id int64) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if slices.Contains(m.ids, id) {
		return false, nil
	}
	m.ids = append(m.ids, id)
	return true, nil
}

func (m *memSubs) RemoveSubscriber(_ context.Context, id int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ids = slices.DeleteFunc(m.ids, func(x int64) bool { return x == id })
	return nil
}

func (m *memSubs) Subscribers(context.Context) ([]int64, error) { return m.list(), nil }

func (m *memSubs) list() []int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Sorted(slices.Values(m.ids))
}

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if ok() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestSendDeliversToTheChat(t *testing.T) {
	b := newBot(t)
	n := New(Options{Token: "TOKEN", ChatID: "42", BaseURL: b.URL, Logger: discard})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go n.Run(ctx)
	n.Send(Message{Operator: "New wallet connected"})
	eventually(t, "the operator message", func() bool { return slices.Equal(b.to("42"), []string{"New wallet connected"}) })
}

func TestBroadcastGivesSubscribersThePublicText(t *testing.T) {
	b := newBot(t)
	b.blocked = "99"
	subs := &memSubs{ids: []int64{1, 7, 42, 99}}
	n := New(Options{Token: "TOKEN", ChatID: "1", BaseURL: b.URL, Subscribers: subs, SendGap: time.Millisecond, Logger: discard})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go n.Run(ctx)

	n.Send(Message{Operator: "New portfolio by 0xabc…def", Public: "New portfolio"})
	eventually(t, "the broadcast", func() bool {
		return slices.Equal(b.to("7"), []string{"New portfolio"}) && slices.Equal(b.to("42"), []string{"New portfolio"}) && !slices.Contains(subs.list(), 99)
	})
	if got := b.to("1"); !slices.Equal(got, []string{"New portfolio by 0xabc…def"}) {
		t.Fatalf("the operator chat gets the full text, once: %q", got)
	}

	// An event with no public text stays with the operator.
	n.Send(Message{Operator: "Re-evaluation failed: upstream 502"})
	eventually(t, "the operator-only message", func() bool { return len(b.to("1")) == 2 })
	if len(b.to("7")) != 1 {
		t.Fatalf("a subscriber got an operator-only event: %q", b.to("7"))
	}
}

func TestStartAndStopManageSubscribers(t *testing.T) {
	b := newBot(t)
	subs := &memSubs{}
	n := New(Options{Token: "TOKEN", ChatID: "1", BaseURL: b.URL, Subscribers: subs, Logger: discard})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go n.Poll(ctx)

	msg := func(chat int, text string) map[string]any {
		return map[string]any{"message": map[string]any{"chat": map[string]any{"id": chat}, "text": text}}
	}
	b.push(msg(42, "/start"))
	b.push(msg(7, "/start@brighterFinanceBot"))
	b.push(msg(7, "/stop"))
	b.push(msg(5, "hello"))
	eventually(t, "the subscriptions", func() bool { return slices.Equal(subs.list(), []int64{42}) && len(b.to("7")) == 2 })
	if got := b.to("7"); got[0] != welcome || got[1] != goodbye {
		t.Fatalf("7 got %q", got)
	}
	if len(b.to("5")) != 0 {
		t.Fatalf("a plain message got a reply: %q", b.to("5"))
	}

	// Blocking the bot unsubscribes too.
	b.push(map[string]any{"my_chat_member": map[string]any{"chat": map[string]any{"id": 42}, "new_chat_member": map[string]any{"status": "kicked"}}})
	eventually(t, "the blocked chat to go", func() bool { return len(subs.list()) == 0 })
	// Each update is handled once: 42 was welcomed exactly once.
	if got := b.to("42"); !slices.Equal(got, []string{welcome}) {
		t.Fatalf("42 got %q", got)
	}
}

func TestDisabledIsANoOp(t *testing.T) {
	var none *Notifier
	none.Send(Message{Operator: "ignored"})
	if none.Enabled() || New(Options{Token: "t"}).Enabled() || New(Options{ChatID: "1"}).Enabled() {
		t.Fatal("a notifier needs both a token and a chat")
	}
	New(Options{Token: "t"}).Send(Message{Operator: "ignored"})

	// Without the operator chat the bot reads no updates either, so a copy
	// holding only the token never competes for them.
	done := make(chan struct{})
	go func() { New(Options{Token: "t", Subscribers: &memSubs{}}).Poll(context.Background()); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("a disabled bot polled for updates")
	}
}

func TestErrorsNeverCarryTheToken(t *testing.T) {
	n := New(Options{Token: "SECRET-TOKEN", ChatID: "1", BaseURL: "http://127.0.0.1:1", Logger: discard})
	err := n.post(context.Background(), "1", "x")
	if err == nil || strings.Contains(err.Error(), "SECRET-TOKEN") {
		t.Fatalf("got %v", err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"ok":false,"description":"Bad Request: chat not found"}`))
	}))
	defer srv.Close()
	n = New(Options{Token: "SECRET-TOKEN", ChatID: "1", BaseURL: srv.URL, Logger: discard})
	err = n.post(context.Background(), "1", "x")
	if err == nil || !strings.Contains(err.Error(), "chat not found") || strings.Contains(err.Error(), "SECRET-TOKEN") {
		t.Fatalf("got %v", err)
	}
}
