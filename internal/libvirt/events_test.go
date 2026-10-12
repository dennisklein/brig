// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package libvirt

import (
	"context"
	"testing"
	"time"

	golibvirt "github.com/digitalocean/go-libvirt"
)

func TestKindOf(t *testing.T) {
	for in, want := range map[golibvirt.DomainEventType]EventKind{
		golibvirt.DomainEventDefined:     EventDefined,
		golibvirt.DomainEventUndefined:   EventUndefined,
		golibvirt.DomainEventStarted:     EventStarted,
		golibvirt.DomainEventSuspended:   EventPaused,
		golibvirt.DomainEventResumed:     EventResumed,
		golibvirt.DomainEventStopped:     EventStopped,
		golibvirt.DomainEventShutdown:    EventOther,
		golibvirt.DomainEventCrashed:     EventOther,
		golibvirt.DomainEventPmsuspended: EventOther,
	} {
		if got := kindOf(in); got != want {
			t.Errorf("kindOf(%d) = %v, want %v", in, got, want)
		}
	}
	for k := EventOther; k <= EventStopped; k++ {
		if k.String() == "" {
			t.Errorf("EventKind(%d) has no name", k)
		}
	}
}

func TestLifecycleEvents(t *testing.T) {
	c, f := newFake(nil)
	f.events = make(chan golibvirt.DomainEventLifecycleMsg)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := c.LifecycleEvents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	send := func(name string, e golibvirt.DomainEventType) {
		f.events <- golibvirt.DomainEventLifecycleMsg{Dom: golibvirt.Domain{Name: name}, Event: int32(e)}
	}
	go send("brig-a", golibvirt.DomainEventStarted)
	select {
	case ev := <-ch:
		if ev != (LifecycleEvent{Domain: "brig-a", Kind: EventStarted}) {
			t.Errorf("event = %+v", ev)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no event")
	}

	// Cancelling must end the stream even with an event in flight.
	sent := make(chan struct{})
	go func() {
		send("brig-a", golibvirt.DomainEventStopped)
		close(sent)
	}()
	<-sent
	cancel()
	close(f.events)
	timeout := time.After(5 * time.Second)
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				return
			}
		case <-timeout:
			t.Fatal("channel not closed after cancel")
		}
	}
}
