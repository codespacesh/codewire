package relay_test

import (
	"testing"
	"time"

	"github.com/codespacesh/codewire/internal/relay"
)

func TestHubRegisterUnregister(t *testing.T) {
	h := relay.NewNodeHub()
	h.Register("n1", nil) // nil sender for test
	if !h.Has("n1") {
		t.Fatal("expected n1 registered")
	}
	h.Unregister("n1")
	if h.Has("n1") {
		t.Fatal("expected n1 unregistered")
	}
}

func TestHubSend(t *testing.T) {
	h := relay.NewNodeHub()
	ch := make(chan relay.HubMessage, 1)
	h.Register("n1", ch)
	err := h.Send("n1", relay.HubMessage{Type: "test"})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case msg := <-ch:
		if msg.Type != "test" {
			t.Fatalf("wrong type: %s", msg.Type)
		}
	case <-time.After(time.Second):
		t.Fatal("timeout")
	}
}

func TestHubSendUnknown(t *testing.T) {
	h := relay.NewNodeHub()
	err := h.Send("missing", relay.HubMessage{Type: "x"})
	if err == nil {
		t.Fatal("expected error for unknown node")
	}
}
