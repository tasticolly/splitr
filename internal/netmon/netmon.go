// Package netmon сообщает о событиях, после которых состояние сети и туннеля
// могло измениться: смена интерфейса или маршрута и пробуждение из сна.
//
// Зачем это вместо опроса по таймеру. Сторож демона обязан узнать о падении
// туннеля как можно раньше, но тикать чаще — значит чаще дёргать pfctl впустую:
// в покое ничего не меняется сутками. Ядро само рассказывает об изменениях
// через маршрутный сокет, поэтому реакция становится мгновенной, а цена в
// простое — нулевой. Тикер при этом остаётся: события — ускоритель, а не
// замена страховке.
package netmon

import (
	"context"
	"sync"
	"time"
)

// Event — повод перепроверить состояние.
type Event struct {
	// Reason — человекочитаемая причина, она уходит в журнал демона.
	Reason string
	// Wake отличает пробуждение из сна от обычной смены сети: после сна
	// нужно не только перепроверить pf, но и заново поднимать туннель,
	// не дожидаясь конца нарастающей задержки переподключения.
	Wake bool
}

// Logf — куда писать диагностику. Совпадает по форме с Daemon.logf.
type Logf func(format string, args ...any)

// Monitor доставляет события в канал C.
type Monitor struct {
	// C — канал событий ёмкостью 1. Отправка неблокирующая: если получатель
	// ещё не разобрал прошлое событие, новое просто отбрасывается — смысл
	// сигнала «перепроверь состояние» от повторов не меняется.
	C <-chan Event

	ch   chan Event
	logf Logf

	// debounce гасит шторм сообщений: при подъёме Wi-Fi ядро присылает
	// десятки RTM_* за доли секунды, и каждое из них не повод для отдельного
	// прохода сторожа.
	debounce time.Duration

	mu       sync.Mutex
	lastSent time.Time
}

// Options — настройки монитора. Нулевое значение даёт разумные умолчания.
type Options struct {
	// Debounce — минимальный промежуток между двумя доставленными событиями.
	Debounce time.Duration
	// SleepGap — насколько настенные часы должны обогнать ожидаемый ход
	// событий, чтобы это считалось сном. Меньше нескольких секунд ставить
	// нельзя: обычная загрузка машины даёт задержки в сотни миллисекунд.
	SleepGap time.Duration
	// WakePoll — как часто проверять часы на предмет пропущенного сна.
	WakePoll time.Duration
}

func (o Options) withDefaults() Options {
	if o.Debounce <= 0 {
		// Секунда — то же значение, на котором остановился netmon у Tailscale.
		// На простаивающей машине маршрутный сокет молчит сутками, так что
		// цена этого выбора — только задержка реакции внутри одной секунды.
		o.Debounce = time.Second
	}
	if o.SleepGap <= 0 {
		o.SleepGap = 15 * time.Second
	}
	if o.WakePoll <= 0 {
		// Опрос часов должен быть заметно чаще порога сна: иначе обычная
		// задержка планировщика читалась бы как сон.
		o.WakePoll = 5 * time.Second
	}
	return o
}

// Start запускает монитор и возвращает его. Все горутины живут до отмены ctx.
//
// Ошибка подписки на события ядра не считается фатальной: демон обязан
// работать и на тикере, просто медленнее реагируя. Поэтому Start ничего
// не возвращает кроме монитора, а о неудаче пишет в журнал.
func Start(ctx context.Context, logf Logf, opts Options) *Monitor {
	opts = opts.withDefaults()
	if logf == nil {
		logf = func(string, ...any) {}
	}
	ch := make(chan Event, 1)
	m := &Monitor{C: ch, ch: ch, logf: logf, debounce: opts.Debounce}

	go m.watchWake(ctx, opts)
	go watchRoutes(ctx, m)
	return m
}

// emit доставляет событие, если с прошлого прошло больше debounce.
// Пробуждение из сна пропускается всегда: пропустить его нельзя, а приходит
// оно редко.
func (m *Monitor) emit(ev Event) {
	if !ev.Wake {
		m.mu.Lock()
		if time.Since(m.lastSent) < m.debounce {
			m.mu.Unlock()
			return
		}
		m.lastSent = time.Now()
		m.mu.Unlock()
	}

	select {
	case m.ch <- ev:
	default:
		// Получатель ещё занят прошлым событием — повторять нечего.
	}
}

// watchWake ловит сон по разрыву между настенными часами и ходом тикера.
//
// Готового способа узнать о пробуждении из Go без Objective-C нет, но он и не
// нужен: во сне тикер не тикает, а календарь идёт. Разрыв между ними и есть
// длительность сна. Побочно так же ловится перевод часов и заморозка процесса —
// перепроверить состояние в обоих случаях правильно.
func (m *Monitor) watchWake(ctx context.Context, opts Options) {
	ticker := time.NewTicker(opts.WakePoll)
	defer ticker.Stop()

	// Round(0) отрезает монотонную часть, оставляя настенные часы: без этого
	// вычитание time.Time дало бы монотонную разницу, и разрыв бы не проявился.
	last := time.Now().Round(0)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			now := time.Now().Round(0)
			gap := now.Sub(last)
			last = now
			if gap >= opts.SleepGap {
				m.logf("looks like a wake from sleep: the clock jumped %s forward, rechecking the network", gap.Round(time.Second))
				m.emit(Event{Reason: "wake from sleep", Wake: true})
			}
		}
	}
}
