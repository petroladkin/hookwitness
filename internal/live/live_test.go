package live

import "testing"

func TestPublishWakesOnlyItsRegister(t *testing.T) {
	b := New()
	a, stopA := b.Subscribe(1)
	other, stopOther := b.Subscribe(2)
	defer stopOther()

	b.Publish(1)
	b.Publish(1) // coalesced with the pending wake-up, never blocks
	select {
	case <-a:
	default:
		t.Fatal("subscriber of register 1 not woken")
	}
	select {
	case <-a:
		t.Fatal("two publishes before a read must coalesce into one wake-up")
	default:
	}
	select {
	case <-other:
		t.Fatal("register 2 woken by a publish on register 1")
	default:
	}

	stopA()
	b.Publish(1) // no subscribers left: must not panic or block
	if len(b.subs[1]) != 0 {
		t.Fatal("unsubscribed channel still registered")
	}
}
