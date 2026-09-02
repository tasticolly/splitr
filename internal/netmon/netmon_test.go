package netmon

import (
	"context"
	"testing"
	"time"
)

func newTestMonitor(debounce time.Duration) *Monitor {
	ch := make(chan Event, 1)
	return &Monitor{C: ch, ch: ch, logf: func(string, ...any) {}, debounce: debounce}
}

func TestEmitDeliversFirstEvent(t *testing.T) {
	t.Parallel()

	m := newTestMonitor(time.Hour)
	m.emit(Event{Reason: "первое"})

	select {
	case ev := <-m.C:
		if ev.Reason != "первое" {
			t.Fatalf("причина %q", ev.Reason)
		}
	default:
		t.Fatal("первое событие обязано дойти без задержки")
	}
}

// Шторм сообщений при подъёме Wi-Fi не должен превращаться в шторм проходов
// сторожа: каждый проход — это несколько запусков pfctl.
func TestEmitCoalescesBurst(t *testing.T) {
	t.Parallel()

	m := newTestMonitor(time.Hour)
	for range 50 {
		m.emit(Event{Reason: "шторм"})
	}

	<-m.C
	select {
	case ev := <-m.C:
		t.Fatalf("из пачки событий должно остаться одно, пришло ещё %+v", ev)
	default:
	}
}

// Пробуждение из сна пропускается мимо гашения: пропустить его нельзя,
// а приходит оно редко.
func TestEmitLetsWakeThroughDebounce(t *testing.T) {
	t.Parallel()

	m := newTestMonitor(time.Hour)
	m.emit(Event{Reason: "смена сети"})
	<-m.C
	m.emit(Event{Reason: "сон", Wake: true})

	select {
	case ev := <-m.C:
		if !ev.Wake {
			t.Fatalf("ожидалось событие пробуждения, пришло %+v", ev)
		}
	default:
		t.Fatal("событие пробуждения потерялось на гашении")
	}
}

// Медленный получатель не должен ни блокировать читателя ядра, ни копить
// очередь: смысл сигнала «перепроверь состояние» от повторов не меняется.
func TestEmitNeverBlocks(t *testing.T) {
	t.Parallel()

	m := newTestMonitor(0)
	done := make(chan struct{})
	go func() {
		for range 1000 {
			m.emit(Event{Reason: "поток"})
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("emit заблокировался на переполненном канале")
	}
}

// Разрыв в настенных часах — единственный способ узнать о сне без Objective-C.
func TestWatchWakeDetectsClockGap(t *testing.T) {
	t.Parallel()

	m := newTestMonitor(time.Hour)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Порог заведомо меньше нуля: любой промежуток между тиками считается сном,
	// иначе тест пришлось бы усыплять на настоящие пятнадцать секунд.
	go m.watchWake(ctx, Options{WakePoll: time.Millisecond, SleepGap: time.Nanosecond})

	select {
	case ev := <-m.C:
		if !ev.Wake {
			t.Fatalf("ожидалось событие пробуждения, пришло %+v", ev)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("разрыв в часах не превратился в событие")
	}
}

func TestOptionsDefaults(t *testing.T) {
	t.Parallel()

	o := Options{}.withDefaults()
	if o.Debounce <= 0 || o.SleepGap <= 0 || o.WakePoll <= 0 {
		t.Fatalf("умолчания не проставлены: %+v", o)
	}
	// Порог сна обязан быть с запасом больше периода опроса, иначе обычная
	// задержка планировщика читалась бы как пробуждение.
	if o.SleepGap < 2*o.WakePoll {
		t.Fatalf("порог сна %s слишком близок к периоду опроса %s", o.SleepGap, o.WakePoll)
	}
}

// Монитор обязан запускаться и останавливаться по контексту на любой платформе:
// там, где событий ядра нет, он просто молчит.
func TestStartStopsWithContext(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	m := Start(ctx, nil, Options{})
	if m == nil || m.C == nil {
		t.Fatal("Start обязан вернуть монитор с каналом")
	}
	cancel()
}
