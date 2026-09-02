import AppKit

/// Владелец пункта в строке меню.
///
/// Вынесен из делегата по одной практической причине: почти все способы
/// «потерять» иконку — это ошибки владения и оформления NSStatusItem, а не
/// логики приложения. Когда они собраны в одном типе, их видно и можно
/// проверить на старте. Делегат теперь просит «покажи такое состояние»
/// и не знает ни про NSStatusBar, ни про вырез экрана.
final class StatusItemController {
    /// Имя для автосохранения позиции. Без него AppKit не запоминает, куда
    /// пользователь перетащил иконку, и при каждом запуске ставит её крайней
    /// слева — то есть первой в очереди на исчезновение под вырезом.
    /// Со списком имя также даёт стабильные ключи в UserDefaults,
    /// которые мы умеем чинить (см. clearStaleVisibility).
    static let autosaveName = "SplitR"

    /// Ширина пункта фиксированная, а не variableLength.
    ///
    /// Это и есть лечение исходной болезни. На ноутбуке с вырезом строка меню
    /// разорвана надвое, и справа от выреза места ровно столько, сколько
    /// осталось от чужих иконок. Пункт переменной ширины с символом 15 pt
    /// занимал 35 точек при 34 свободных — не помещался, и система убирала
    /// его под вырез, где иконку не видно вообще.
    ///
    /// 18 — это ширина содержимого; окно пункта система делает на 16 точек
    /// шире (по 8 полей с каждой стороны), то есть 34 точки на экране. Ровно
    /// столько же занимают системные пункты Пункта управления и большинство
    /// чужих иконок, так что размер не «ужатый», а обычный — просто прежние
    /// 15 pt были на две точки крупнее нормы, и этих двух точек не хватило.
    private static let length: CGFloat = 18

    /// Сильная ссылка обязательна: NSStatusBar держит пункт слабо, и стоит
    /// последней ссылке уйти — иконка молча пропадает из строки меню.
    private let statusItem: NSStatusItem

    init(menu: NSMenu) {
        Self.clearStaleVisibility()

        statusItem = NSStatusBar.system.statusItem(withLength: Self.length)
        statusItem.autosaveName = Self.autosaveName
        // Пункт нельзя утащить из строки меню cmd-драгом: пользователь
        // и так не видит иконку, а «случайно выбросил» — это ещё один
        // способ навсегда потерять единственный интерфейс приложения.
        statusItem.behavior = []
        // Явно и безусловно. Система хранит признак видимости сама, и один
        // случайный cmd-драг иконки за пределы строки меню делает приложение
        // невидимым навсегда — при том, что процесс живёт и всё работает.
        statusItem.isVisible = true
        statusItem.menu = menu

        // Картинку ставим прямо при добавлении пункта, а не следующим кадром:
        // если сначала показать пустую кнопку, WindowServer успевает разложить
        // строку меню по её нулевой ширине, и последующая установка image
        // перекраивает уже готовую раскладку (тот же приём в alt-tab,
        // src/Menubar.swift — «apply icon prefs eagerly while the status item
        // is still being added»).
        render(.daemonDown, tooltip: "SplitR")

        // Если система или пользователь всё-таки погасят пункт — вернём его
        // и запишем это. Токен наблюдения обязан жить: выброшенный в «_»,
        // он отписывается сразу, и событие никогда не придёт.
        visibilityObserver = statusItem.observe(\.isVisible, options: [.new]) { item, _ in
            guard !item.isVisible else { return }
            Diag.log("the status item was hidden from outside — putting it back")
            item.isVisible = true
        }
    }

    private var visibilityObserver: NSKeyValueObservation?

    func render(_ state: GuardState, tooltip: String) {
        guard let button = statusItem.button else {
            Diag.log("ERROR: NSStatusItem has no button — nothing to draw")
            return
        }
        if let image = StatusIcon.image(for: state, appearance: button.effectiveAppearance) {
            statusItem.length = Self.length
            button.image = image
            button.title = ""
            button.imagePosition = .imageOnly
        } else {
            // Символа нет — значит, SF Symbol недоступен в этой версии macOS.
            // Пустая кнопка выглядела бы как отсутствующее приложение, поэтому
            // рисуем текстом: невзрачно, но видно и кликабельно.
            Diag.log("symbol \"\(state.symbol)\" is unavailable, falling back to the text badge \"\(state.badge)\"")
            // Под текст фиксированной ширины не хватит: «OFF» шире иконки.
            // Здесь лучше вылезти за отведённые 18 точек, чем показать обрезок.
            statusItem.length = NSStatusItem.variableLength
            button.image = nil
            // Цвет здесь динамический и разрешается при отрисовке самой
            // кнопкой, поэтому оформление указывать не нужно — в отличие от
            // картинки, которую мы собираем заранее.
            button.attributedTitle = NSAttributedString(
                string: state.badge,
                attributes: [.font: NSFont.systemFont(ofSize: 12, weight: .bold),
                             .foregroundColor: state.tint])
            button.imagePosition = .noImage
        }
        // Пульсация — единственный способ показать «идёт работа» тому, кто
        // смотрит на строку меню и меню не открывает. Включаем и гасим её
        // только здесь: состояние иконки и её анимация обязаны меняться
        // одним действием, иначе рано или поздно останется анимация без
        // состояния — то есть вечно мигающая иконка.
        if state.isTransient { startPulse() } else { stopPulse() }
        button.toolTip = tooltip
        // Иконка — единственный носитель состояния для тех, кто не открывает
        // меню, поэтому состояние обязано читаться и VoiceOver'ом.
        button.setAccessibilityLabel("SplitR: " + state.title)
    }

    // MARK: - пульсация

    /// Таймер живёт только пока идёт переходное состояние. Отдельный таймер,
    /// а не общий с опросом: опрос идёт раз в две секунды — на этой частоте
    /// мигание неотличимо от подёргивания.
    private var pulseTimer: Timer?
    private var dimmed = false

    private func startPulse() {
        // Опрос перерисовывает иконку каждые полсекунды, и без этой проверки
        // таймер пересоздавался бы на каждом тике, теряя фазу мигания.
        guard pulseTimer == nil else { return }
        Diag.log("icon pulse: on")
        let t = Timer(timeInterval: 0.6, repeats: true) { [weak self] _ in self?.pulse() }
        // .common — иначе мигание замирает на открытом меню, ровно тогда,
        // когда человек на него и смотрит.
        RunLoop.main.add(t, forMode: .common)
        pulseTimer = t
        pulse()
    }

    /// Нижняя точка пульсации. Не «поглуше, чтобы заметнее мигало»: значок
    /// на просвет смешивается с фоном строки меню, и на светлой теме
    /// прозрачность 0.35 роняет его контраст до 1.6:1 — половину каждого цикла
    /// значка попросту не видно. 0.75 держит 3:1 (нижний предел читаемости
    /// для значимых элементов) в обеих темах и остаётся заметным движением.
    private static let dimAlpha: CGFloat = 0.75

    private func pulse() {
        guard let button = statusItem.button else { return }
        dimmed.toggle()
        NSAnimationContext.runAnimationGroup { ctx in
            ctx.duration = 0.5
            button.animator().alphaValue = dimmed ? Self.dimAlpha : 1.0
        }
    }

    /// Гасит анимацию и обязательно возвращает полную непрозрачность:
    /// иначе иконка могла бы остаться навсегда полупрозрачной — то есть
    /// выглядеть выключенной при работающей защите.
    func stopPulse() {
        guard pulseTimer != nil else { return }
        Diag.log("icon pulse: off")
        pulseTimer?.invalidate()
        pulseTimer = nil
        dimmed = false
        // Снимаем незавершённую анимацию: без этого начатое затухание
        // доиграет и оставит кнопку прозрачной.
        statusItem.button?.layer?.removeAllAnimations()
        statusItem.button?.alphaValue = 1.0
    }

    deinit {
        pulseTimer?.invalidate()
    }

    // MARK: - самопроверка

    /// Проверяет, что пункт действительно оказался на экране, и пишет вывод
    /// в stderr. Вызывать имеет смысл не сразу: система расставляет пункты
    /// асинхронно, и сразу после создания окно кнопки ещё нулевого размера.
    /// Итог проверки размещения: nil — всё в порядке, иначе объяснение
    /// для пользователя. Заполняется в verifyPlacement().
    private(set) var placementProblem: String?

    func verifyPlacement() {
        placementProblem = nil
        guard let button = statusItem.button else {
            Diag.log("CHECK: no button — there is no icon")
            placementProblem = "The system did not grant a slot in the menu bar."
            return
        }
        Diag.log("placement check:")
        Diag.log("  isVisible: \(statusItem.isVisible), length: \(statusItem.length)")
        Diag.log("  content: image=\(button.image == nil ? "none" : "yes"), title=\"\(button.title)\"")
        if button.image == nil && button.title.isEmpty {
            Diag.log("  ERROR: the button is empty — the item takes space but draws nothing")
        }
        guard let window = button.window else {
            Diag.log("  ERROR: the button has no window — the system granted no menu bar slot")
            return
        }
        let frame = window.frame
        Diag.log("  on screen: x \(Int(frame.minX))…\(Int(frame.maxX)), width \(Int(frame.width))")
        guard let screen = window.screen ?? NSScreen.main else { return }
        guard let left = screen.auxiliaryTopLeftArea, let right = screen.auxiliaryTopRightArea else {
            Diag.log("  screen has no notch — the item is visible if it fitted")
            return
        }
        let notch = CGRect(x: left.maxX, y: left.minY, width: right.minX - left.maxX, height: left.height)
        Diag.log("  screen notch: x \(Int(notch.minX))…\(Int(notch.maxX))")
        if frame.minX < notch.maxX && frame.maxX > notch.minX {
            Diag.log("  ERROR: the item ended up under the notch — the icon is invisible.")
            Diag.log("         The menu bar is full: no room left right of the notch.")
            Diag.log("         Remove a couple of other icons or install Ice/Bartender.")
            placementProblem = """
                Иконка splitr есть, но macOS задвинула её под вырез экрана:                 в строке меню справа от выреза не осталось свободного места.

                Убери из строки меню одну-две иконки других приложений —                 splitr появится сам. Либо поставь менеджер строки меню                 (Ice, Bartender), он умеет показывать спрятанное.
                """
        } else {
            Diag.log("  ok: the item sits right of the notch, the icon is visible")
        }
    }

    /// Снимок нарисованной кнопки в PNG. Нужен ровно для проверки «иконка
    /// действительно рисуется, а не просто занимает место»: снаружи это
    /// не проверить без разрешения на запись экрана, а сама себя кнопка
    /// отрисовать может всегда.
    func dumpRenderedIcon(to path: String) {
        guard let button = statusItem.button,
              let rep = button.bitmapImageRepForCachingDisplay(in: button.bounds) else { return }
        button.cacheDisplay(in: button.bounds, to: rep)
        guard let png = rep.representation(using: .png, properties: [:]) else { return }
        try? png.write(to: URL(fileURLWithPath: path))
        Diag.log("button snapshot written: \(path)")
    }

    // MARK: -

    /// Снимает запомненное системой «скрыто».
    ///
    /// AppKit сам хранит видимость пункта в UserDefaults под ключом
    /// «NSStatusItem Visible <autosaveName>». Стоит один раз утащить иконку
    /// из строки меню (или системе — решить это за пользователя), и ключ
    /// остаётся навсегда: приложение запускается, работает, но не показывается,
    /// и переустановка не помогает, потому что настройки переустановку переживают.
    /// Тот же приём применяет SwiftBar (removeStatusItemVisibilityKeys).
    private static func clearStaleVisibility() {
        let defaults = UserDefaults.standard
        let keys = defaults.dictionaryRepresentation().keys.filter {
            $0.hasPrefix("NSStatusItem Visible")
        }
        for key in keys where defaults.bool(forKey: key) == false {
            Diag.log("clearing remembered \"hidden\": \(key)")
            defaults.removeObject(forKey: key)
        }

        // Часть этих ключей система пишет не в домен приложения, а в общие
        // настройки конкретной машины (~/Library/Preferences/ByHost/
        // .GlobalPreferences.<UUID>.plist). Через UserDefaults.standard их не
        // видно и не стереть, поэтому лезем в CFPreferences напрямую.
        let global = ".GlobalPreferences" as CFString
        let names = CFPreferencesCopyKeyList(global, kCFPreferencesCurrentUser,
                                             kCFPreferencesCurrentHost) as? [String] ?? []
        for key in names where key.hasPrefix("NSStatusItem Visible") && key.hasSuffix(autosaveName) {
            let value = CFPreferencesCopyValue(key as CFString, global, kCFPreferencesCurrentUser,
                                               kCFPreferencesCurrentHost) as? Bool
            guard value == false else { continue }
            Diag.log("clearing remembered \"hidden\" (ByHost): \(key)")
            CFPreferencesSetValue(key as CFString, nil, global, kCFPreferencesCurrentUser,
                                  kCFPreferencesCurrentHost)
        }
        CFPreferencesSynchronize(global, kCFPreferencesCurrentUser, kCFPreferencesCurrentHost)
    }
}
