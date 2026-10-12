// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package libvirt

import (
	"context"
	"fmt"

	golibvirt "github.com/digitalocean/go-libvirt"
)

// EventKind is what happened to a domain.
type EventKind int

// Kinds of lifecycle events.
const (
	// EventOther covers events that do not change what brig shows, such as
	// a guest that has begun to shut down or crashed.
	EventOther EventKind = iota
	EventDefined
	EventUndefined
	EventStarted
	// EventPaused means that the guest was suspended.
	EventPaused
	EventResumed
	// EventStopped means that the domain shut off.
	EventStopped
)

func (k EventKind) String() string {
	switch k {
	case EventDefined:
		return "defined"
	case EventUndefined:
		return "undefined"
	case EventStarted:
		return "started"
	case EventPaused:
		return "paused"
	case EventResumed:
		return "resumed"
	case EventStopped:
		return "stopped"
	default:
		return "other"
	}
}

// LifecycleEvent reports that the state or definition of a domain changed.
type LifecycleEvent struct {
	// Domain is the name of the domain, which libvirt also reports for
	// domains that brig did not create.
	Domain string
	Kind   EventKind
}

func kindOf(e golibvirt.DomainEventType) EventKind {
	switch e {
	case golibvirt.DomainEventDefined:
		return EventDefined
	case golibvirt.DomainEventUndefined:
		return EventUndefined
	case golibvirt.DomainEventStarted:
		return EventStarted
	case golibvirt.DomainEventSuspended:
		return EventPaused
	case golibvirt.DomainEventResumed:
		return EventResumed
	case golibvirt.DomainEventStopped:
		return EventStopped
	default:
		return EventOther
	}
}

// LifecycleEvents streams the lifecycle events of all domains until ctx ends
// or the connection to the daemon is lost, when it closes the channel. A
// state read after an event is current, but events may be missed between the
// loss of the connection and a new call, so consumers poll as well.
func (c *Conn) LifecycleEvents(ctx context.Context) (<-chan LifecycleEvent, error) {
	in, err := c.c.LifecycleEvents(ctx)
	if err != nil {
		return nil, fmt.Errorf("subscribe to domain events: %w", err)
	}
	out := make(chan LifecycleEvent)
	go func() {
		defer close(out)
		for m := range in {
			ev := LifecycleEvent{Domain: m.Dom.Name, Kind: kindOf(golibvirt.DomainEventType(m.Event))}
			select {
			case out <- ev:
			case <-ctx.Done():
				// Drain, so that go-libvirt can end its stream.
				for range in {
				}
				return
			}
		}
	}()
	return out, nil
}
