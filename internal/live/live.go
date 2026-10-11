// Package live tells open live feeds that a register has new requests.
//
// A notification carries no data, only a wake-up: the feed then reads what
// it has not sent yet from the store. A slow feed therefore never loses a
// request and never gets one twice, however many notifications it missed.
package live

import "sync"

// Broker fans notifications out to the feeds subscribed to a register.
type Broker struct {
	mu   sync.Mutex
	subs map[int64]map[chan struct{}]struct{}
}

// New returns an empty Broker.
func New() *Broker {
	return &Broker{subs: map[int64]map[chan struct{}]struct{}{}}
}

// Subscribe returns a channel that receives a value after Publish on
// register, and a function that ends the subscription.
func (b *Broker) Subscribe(register int64) (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	b.mu.Lock()
	if b.subs[register] == nil {
		b.subs[register] = map[chan struct{}]struct{}{}
	}
	b.subs[register][ch] = struct{}{}
	b.mu.Unlock()
	return ch, func() {
		b.mu.Lock()
		delete(b.subs[register], ch)
		if len(b.subs[register]) == 0 {
			delete(b.subs, register)
		}
		b.mu.Unlock()
	}
}

// Publish wakes every feed of register. It never blocks: a feed that has a
// wake-up pending already will read the new request with the earlier one.
func (b *Broker) Publish(register int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for ch := range b.subs[register] {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}
