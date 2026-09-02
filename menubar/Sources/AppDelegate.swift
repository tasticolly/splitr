import AppKit
import UserNotifications

/// Команда для ручного подъёма демона. Приложение пользовательское,
/// sudo из него не запросить, поэтому единственное честное поведение —
/// показать команду и положить её в буфер обмена.
let kickstartCommand = "sudo launchctl kickstart -k system/com.splitr.daemon"

/// Связывает три части: опрос демона (GuardAPI), показ состояния
/// (StatusItemController) и меню (MenuBuilder). Сам он ничего не рисует
/// и ничего не форматирует — только хранит последний снимок состояния
/// и исполняет команды пользователя.
final class AppDelegate: NSObject, NSApplicationDelegate, NSMenuDelegate {
    private let api: GuardAPI
    private let notifier = Notifier()
    private let rulesWindow = TextWindow()
    private let logWindow = TextWindow()
    private let blockedWindow = TextWindow()
    private let configWindow = TextWindow()
    private let updateWindow = TextWindow()
    private let blockedStream = BlockedStream()

    private var statusItem: StatusItemController?
    private var timer: Timer?

    /// Последний удачный ответ демона и последняя ошибка. Держим оба:
    /// когда демон отвалился, старые цифры уже врут, и меню должно
    /// показывать причину, а не подмороженный снимок.
    private var status: DaemonStatus?
    private var lastError: APIError?
    private var config: DaemonConfig?
    /// Флаг «запрос в полёте»: тик таймера не должен ставить в очередь
    /// второй опрос, пока первый ещё не вернулся.
    private var polling = false
    /// Начатая, но не подтверждённая операция с туннелем. Пока она есть,
    /// приложение показывает её, а не то, что говорит демон: демон о клике
    /// узнаёт лишь со следующим ответом, а человеку реакция нужна сразу.
    private var pending: PendingOperation? {
        didSet { pendingChanged(from: oldValue) }
    }
    /// Команда демону, ответ на которую ещё не пришёл. Нужна только меню:
    /// пока она не вернулась, повторный клик по тем же пунктам бессмыслен.
    private var busy: String? {
        didSet { busySince = busy == nil ? .distantPast : Date() }
    }
    private var busySince = Date.distantPast
    /// То же, но с предохранителем: команда, о которой мы почему-то не узнали
    /// результат, не имеет права выключить меню навсегда.
    private var activeBusy: String? {
        guard Date().timeIntervalSince(busySince) < PendingOperation.timeout else { return nil }
        return busy
    }
    /// Ссылка на вход, о которой мы уже сказали. Демон повторяет требование в
    /// каждом ответе, а показывать одно и то же окно раз в полсекунды нельзя.
    private var reportedAuthURL: String?
    /// Текст последней выполненной команды — показывается в шапке меню,
    /// чтобы результат клика был виден без всплывающих окон. Живёт недолго:
    /// «конфиг перечитан» через час после клика — уже не отчёт, а мусор,
    /// который отвлекает от состояния защиты.
    private var lastAction: String? {
        didSet { lastActionAt = Date() }
    }
    private var lastActionAt = Date.distantPast
    private static let lastActionLifetime: TimeInterval = 30

    /// Отчёт о последней команде, пока он ещё что-то значит.
    private var freshAction: String? {
        guard Date().timeIntervalSince(lastActionAt) < Self.lastActionLifetime else { return nil }
        return lastAction
    }

    /// Адрес управляющего API. По умолчанию — тот, что демон открывает сам;
    /// переменная окружения нужна для отладки против подставного демона
    /// (тот же приём, что и SPLITR_DUMP_ICON ниже).
    static var apiAddress: String {
        ProcessInfo.processInfo.environment["SPLITR_API_ADDR"] ?? "127.0.0.1:8787"
    }

    init(api: GuardAPI = APIClient(host: AppDelegate.apiAddress)) {
        self.api = api
        super.init()
    }

    /// Что показывать прямо сейчас. Свойство нарочно без побочных действий:
    /// его читают и меню, и иконка, и снятие просроченной операции отсюда
    /// означало бы модальное окно из середины отрисовки меню.
    private var state: GuardState {
        // Оптимистичное состояние перебивает ответ демона, но только пока не
        // вышел срок: защита от «залипания» важнее красивого перехода.
        // Просроченную операцию снимает tick(), здесь мы её просто не
        // показываем — на случай, если чтение случилось раньше снятия.
        if let p = pending, !p.expired() { return p.state }
        guard let status else { return .daemonDown }
        return GuardState.from(status)
    }

    func applicationDidFinishLaunching(_ notification: Notification) {
        Diag.reportEnvironment()
        Diag.log("login item: " + LoginItem.describe())

        let menu = NSMenu()
        // Автовключение пунктов отключено: иначе AppKit сам решает,
        // что доступно, и затирает наш явный isEnabled (например, у
        // «Disconnect», когда опускать нечего).
        menu.autoenablesItems = false
        menu.delegate = self // пункты строим в menuNeedsUpdate — так они всегда свежие

        let item = StatusItemController(menu: menu)
        statusItem = item
        render()

        // Система расставляет пункты строки меню асинхронно: сразу после
        // создания окно кнопки ещё нулевого размера, и проверять положение
        // бессмысленно. Полсекунды — с запасом.
        DispatchQueue.main.asyncAfter(deadline: .now() + 0.5) { [weak self] in
            self?.statusItem?.verifyPlacement()
            if let path = ProcessInfo.processInfo.environment["SPLITR_DUMP_ICON"] {
                self?.statusItem?.dumpRenderedIcon(to: path)
            }
        }

        refresh()
        loadConfig()

        schedulePolling()
    }

    /// Опрос демона. Интервал зависит от того, ждём ли мы подтверждения:
    /// пока идёт подключение, полсекунды решают, увидит ли человек «protected»
    /// сразу после реального подъёма или через две секунды после него.
    /// В покое частый опрос демону не нужен и батарею тратит зря.
    private func schedulePolling() {
        let interval: TimeInterval = pending == nil ? 2.0 : 0.5
        guard timer?.timeInterval != interval else { return }
        timer?.invalidate()
        // Таймер живёт столько же, сколько приложение; .common — чтобы опрос
        // не замирал, пока открыто меню (иначе иконка «залипает» на открытом меню).
        let t = Timer(timeInterval: interval, repeats: true) { [weak self] _ in self?.tick() }
        RunLoop.main.add(t, forMode: .common)
        timer = t
    }

    /// Тик опроса. Срок операции проверяем до запроса и независимо от него:
    /// иначе молчащий демон (запрос не вернулся, ответа нет) оставлял бы
    /// «подключение» на экране навсегда — а именно это состояние и обязано
    /// заканчиваться при любом развитии событий.
    private func tick() {
        if let p = pending, p.expired() { finishPending(.timedOut("")) }
        refresh()
    }

    /// Приложение уже работает, а пользователь запустил его ещё раз.
    /// В приложении без окон это почти всегда означает одно: «я не нашёл
    /// иконку и решил, что оно не запустилось». Единственный уместный момент,
    /// чтобы объясниться словами. Приём взят у SwiftBar
    /// (applicationShouldHandleReopen → shouldShowMenuBarRecovery).
    func applicationShouldHandleReopen(_ sender: NSApplication, hasVisibleWindows: Bool) -> Bool {
        guard !hasVisibleWindows else { return true }
        statusItem?.verifyPlacement()
        if let problem = statusItem?.placementProblem {
            alert(title: "SplitR is already running, but the icon is not visible", text: problem, style: .warning)
        } else {
            alert(title: "SplitR is already running",
                  text: "The icon is in the menu bar, left of the other icons: \(state.title).",
                  style: .informational)
        }
        return true
    }

    func applicationWillTerminate(_ notification: Notification) {
        timer?.invalidate()
        timer = nil
        statusItem?.stopPulse()
        blockedStream.stop()
    }

    // MARK: - опрос

    private func refresh() {
        guard !polling else { return }
        polling = true
        api.status { [weak self] result in
            guard let self else { return }
            self.polling = false
            switch result {
            case .success(let st):
                let previous = self.status
                self.status = st
                self.lastError = nil
                // Конфиг перечитываем не только когда профилей нет: его меняют
                // и мимо приложения (правкой файла с sudo), а от него зависит,
                // какие пункты меню активны. Раз в минуту — незаметно для
                // демона и достаточно, чтобы меню не врало долго.
                self.pollsSinceConfig += 1
                if self.config?.profiles.isEmpty ?? true
                    || self.pollsSinceConfig >= 30
                    || previous?.configPath != st.configPath {
                    self.loadConfig()
                }
                self.checkPending(against: st)
            case .failure(let err):
                self.status = nil
                self.lastError = err
                // Молчащий демон не отменяет начатую операцию: он мог просто
                // не ответить за отведённые три секунды, пока поднимает
                // туннель. Всё равно эта ветка кончится — по сроку операции.
            }
            self.render()
        }
    }

    /// Сколько опросов прошло с последнего чтения конфига.
    private var pollsSinceConfig = 0

    /// Идущее обновление и его текущий шаг. Пока updater жив, вторую сборку
    /// запустить нельзя, а меню показывает шаг вместо кнопки.
    private var updater: Updater?
    private var updateStage: String?

    private func loadConfig() {
        pollsSinceConfig = 0
        api.config { [weak self] result in
            if case .success(let cfg) = result { self?.config = cfg }
        }
    }

    private func render() {
        let s = state
        statusItem?.render(s, tooltip: StatusText.tooltip(state: s, status: status))
        notifier.observe(s)
    }

    // MARK: - оптимистичное состояние

    /// Клик принят: показываем это немедленно, не дожидаясь ни ответа на POST,
    /// ни следующего опроса. Всё остальное — частый опрос и погашенные пункты
    /// меню — следствия того, что операция началась, поэтому и делаются здесь.
    private func begin(_ op: PendingOperation) {
        pending = op
        lastAction = op.label
    }

    /// Начало команды, которой хватает надписи в меню.
    ///
    /// Возвращает false ровно в одном случае — предыдущая команда ещё не
    /// ответила; её пункты в меню в этот момент и так серые, так что молчащий
    /// отказ никого не застаёт. Идущий подъём туннеля командам не мешает:
    /// защита и политика живут в pf и с sshuttle не спорят, а отказывать
    /// молча целых 45 секунд — это снова клик без ответа.
    /// Проверяем activeBusy, а не busy: команда, чей ответ потерялся,
    /// не имеет права запретить все остальные навсегда.
    @discardableResult
    private func begin(busy label: String) -> Bool {
        guard activeBusy == nil else { return false }
        busy = label
        lastAction = label
        render()
        return true
    }

    /// Появление и исчезновение операции меняет и частоту опроса, и картинку.
    /// Промежуточные правки самой операции (sawDaemonStart) не меняют ничего.
    private func pendingChanged(from old: PendingOperation?) {
        guard (old == nil) != (pending == nil) else { return }
        Diag.log("pending: " + (pending.map { $0.label } ?? "none"))
        schedulePolling()
        render()
    }

    /// Ответ на сам POST /up или /down.
    ///
    /// Успех здесь ещё не означает поднятый туннель: демон отвечает, как только
    /// запустил sshuttle, и в ответе стоит tunnel: starting. Решает не он,
    /// а последующий опрос — поэтому ответ только обновляет снимок состояния.
    private func answered(_ r: Result<DaemonStatus, APIError>) {
        switch r {
        case .success(let st):
            status = st
            lastError = nil
            checkPending(against: st)
            render()
        case .failure(let e):
            // «Демон не ответил вовремя» — не отказ: подъём туннеля он делает
            // до ответа, и запрос вполне может не уложиться в свой таймаут,
            // пока туннель на самом деле поднимается. Такую ветку закрывает
            // опрос или срок операции, а не отмена по первому молчанию.
            if e.isUnreachable {
                lastAction = (pending?.label ?? "") + " (the daemon has not answered yet)"
                render()
                return
            }
            finishPending(.failed(e.errorDescription ?? "unknown error"))
        }
    }

    /// Сверяет начатую операцию со свежим ответом демона.
    private func checkPending(against st: DaemonStatus) {
        reportActionRequired(st.actionRequired)
        guard var p = pending else { return }
        if st.tunnelIsStarting && !p.sawDaemonStart {
            p.sawDaemonStart = true
            pending = p
        }
        guard let outcome = p.outcome(for: st) else { return }
        finishPending(outcome)
    }

    /// Снимает оптимистичное состояние и отчитывается о результате.
    /// После неё приложение снова показывает ровно то, что говорит демон.
    private func finishPending(_ outcome: PendingOutcome) {
        guard let p = pending else { return }
        pending = nil
        switch outcome {
        case .succeeded(let text):
            lastAction = text
        case .failed(let text):
            lastAction = "error: " + text
            alert(title: p.kind == .connecting ? "The tunnel did not come up" : "The tunnel did not go down",
                  text: text, style: .warning)
        case .needsAction(let action):
            lastAction = action.text
            reportActionRequired(action)
        case .timedOut(let text):
            let reason = text.isEmpty
                ? "the daemon did not confirm it within \(Int(PendingOperation.timeout)) seconds"
                : text
            lastAction = reason
            // Молчащий демон и так виден по иконке, а вот отвечающий демон,
            // который не довёл дело до конца, — нет.
            Diag.log("pending timed out, status: \(status == nil ? "nil" : "present")")
            if status != nil {
                alert(title: p.kind == .connecting ? "Still no tunnel" : "The tunnel is still up",
                      text: reason + ". SplitR is showing what the daemon reports now.",
                      style: .warning)
            }
        }
        render()
    }

    /// Показывает требование войти по ссылке — по одному разу на ссылку.
    /// Демон повторяет его в каждом ответе, и без этой памяти окно вылезало бы
    /// заново каждые полсекунды.
    private func reportActionRequired(_ action: ActionRequired?) {
        guard let action else {
            reportedAuthURL = nil
            return
        }
        guard action.url != reportedAuthURL else { return }
        reportedAuthURL = action.url
        lastAction = action.text
        guard let link = action.link else {
            alert(title: "SplitR needs you to act", text: action.text, style: .warning)
            return
        }
        if whenMenuClosed({ [weak self] in self?.askToSignIn(action, link) }) { return }
        askToSignIn(action, link)
    }

    /// Предложение открыть ссылку входа. Кнопка «Open the link» здесь
    /// обязательна: сообщение «нужно заново войти» без способа войти —
    /// это тупик, из которого человек выходит перезапуском наугад.
    private func askToSignIn(_ action: ActionRequired, _ link: URL) {
        let a = NSAlert()
        a.alertStyle = .warning
        a.messageText = "Sign in to bring the tunnel up"
        a.informativeText = action.text + "\n\n" + action.url
        a.addButton(withTitle: "Open the link")
        a.addButton(withTitle: "Later")
        NSApp.activate(ignoringOtherApps: true)
        if a.runModal() == .alertFirstButtonReturn { NSWorkspace.shared.open(link) }
    }

    // MARK: - меню

    func menuNeedsUpdate(_ menu: NSMenu) {
        let model = MenuModel(state: state, status: status, config: config,
                              lastError: lastError, lastAction: freshAction,
                              socketPresent: FileManager.default.fileExists(atPath: socketPath()),
                              updateStage: updateStage, pending: pending, busy: activeBusy)
        MenuBuilder.build(into: menu, model: model, target: self, selector: #selector(menuActionClicked(_:)))
    }

    /// Единственная точка входа для всех пунктов меню. Раньше каждый пункт нёс
    /// свой селектор, и делегат обрастал десятком почти одинаковых @objc-обёрток;
    /// теперь список команд задан перечислением, и забыть обработать новую нельзя —
    /// switch без default не соберётся.
    @objc private func menuActionClicked(_ sender: NSMenuItem) {
        guard let box = sender.representedObject as? MenuActionBox else { return }
        switch box.action {
        case .up(let profile):
            // Второй POST /up ничего не ускоряет, зато демон на нём гасит
            // собственный поднимающийся туннель и начинает заново. Именно так
            // и выглядел «потыкать ещё раз, раз не реагирует».
            guard pending == nil else { return }
            begin(PendingOperation(kind: .connecting, profile: profile))
            api.up(profile: profile) { [weak self] r in self?.answered(r) }
        case .down:
            guard pending == nil else { return }
            begin(PendingOperation(kind: .disconnecting))
            api.down { [weak self] r in self?.answered(r) }
        case .protection(let mode):
            setProtection(mode)
        case .policy(let policy):
            setPolicy(policy)
        case .probe:
            probe()
        case .editConfig:
            editConfig()
        case .showRules:
            showRules()
        case .reload:
            guard begin(busy: "reloading the configuration…") else { return }
            api.reload { [weak self] r in
                self?.loadConfig()
                self?.handle(r, success: "configuration reloaded")
            }
        case .openConfig:
            openConfig()
        case .showBlocked:
            showBlocked()
        case .showLog:
            showLog()
        case .openAuthURL(let raw):
            if let url = URL(string: raw) { NSWorkspace.shared.open(url) }
        case .openWeb:
            let addr = config?.httpAddr ?? AppDelegate.apiAddress
            if let url = URL(string: "http://\(addr)") { NSWorkspace.shared.open(url) }
        case .showKickstart:
            showKickstart()
        case .copyKickstart:
            copyKickstart()
        case .refresh:
            refresh()
        case .update:
            startUpdate()
        case .checkUpdate:
            checkUpdate()
        case .showUpdateProgress:
            updateWindow.show(title: "SplitR — update", text: updateLog)
        case .quit:
            NSApp.terminate(nil)
        }
    }

    // MARK: - действия

    private func setProtection(_ mode: String) {
        switch mode {
        case "off":
            // Выключение защиты — единственное действие, после которого трафик
            // может уйти мимо туннеля. Спрашиваем явно.
            guard confirm(title: "Turn protection off?",
                          text: "Nothing will be blocked any more. Whenever the tunnel is down — including if the current one drops on its own — requests to protected routes will go out over the plain internet and expose your real address.",
                          confirmTitle: "Turn off", destructive: true) else { return }
        case "strict":
            guard confirm(title: "Switch protection to strict?",
                          text: "Protected routes will be blocked at all times, even while the tunnel is up. Every open connection to them will be dropped.",
                          confirmTitle: "Switch to strict", destructive: true) else { return }
        default:
            break
        }
        let done = ["on": "protection is on", "off": "protection is off",
                    "strict": "protection is strict"][mode] ?? "protection: \(mode)"
        // Смена защиты — это pfctl на стороне демона: обычно доля секунды, но
        // ответ всё же не мгновенный, и до него меню должно показывать, что
        // команда принята.
        guard begin(busy: "changing protection…") else { return }
        api.protection(mode: mode, policy: nil) { [weak self] r in self?.handle(r, success: done) }
    }

    private func setPolicy(_ policy: String) {
        if policy == "off" {
            guard confirm(title: "Stop protecting any routes?",
                          text: "The rules will stop blocking anything. While the tunnel is down, requests to protected routes will go out over the plain internet and expose your real address.",
                          confirmTitle: "Switch", destructive: true) else { return }
        }
        guard begin(busy: "changing protected routes…") else { return }
        api.protection(mode: nil, policy: policy) { [weak self] r in
            self?.handle(r, success: "protected routes: \(StatusText.policy(policy))")
        }
    }

    /// Правка конфига прямо из меню. Запись идёт через unix-сокет
    /// (см. SocketClient): по TCP демон её запрещает.
    private func editConfig() {
        api.configRaw { [weak self] r in
            guard let self else { return }
            switch r {
            case .failure(let e):
                self.fail(e)
            case .success(let yaml):
                self.configWindow.showEditor(
                    title: "SplitR — configuration (\(self.status?.configPath ?? ""))",
                    text: yaml
                ) { [weak self] edited, done in
                    guard let self else { return }
                    self.api.writeConfig(edited, socketPath: self.config?.socketPath ?? SocketClient.defaultPath) { res in
                        switch res {
                        case .success(let st):
                            self.status = st
                            self.lastError = nil
                            self.lastAction = "configuration saved and reloaded"
                            self.loadConfig()
                            self.render()
                            done(true, "Saved and reloaded.")
                        case .failure(let e):
                            // Ошибку валидации показываем в самом окне:
                            // закрывать редактор с несохранённой правкой — худшее,
                            // что можно сделать с человеком, который её набрал.
                            done(false, e.errorDescription ?? "could not save")
                        }
                    }
                }
            }
        }
    }

    private func probe() {
        // Проверка ходит по сети и занимает секунды — без пометки «идёт» меню
        // на второй клик отвечало бы второй такой же проверкой.
        guard begin(busy: "testing protection…") else { return }
        api.probe { [weak self] r in
            guard let self else { return }
            self.busy = nil
            switch r {
            case .success(let rep):
                self.lastAction = rep.verdict
                let (title, body) = rep.report
                self.alert(title: title, text: body,
                           style: (rep.leaked && !rep.tunnelUp) ? .critical : .informational)
            case .failure(let e):
                self.fail(e)
            }
            self.refresh()
        }
    }

    private func showRules() {
        api.rules { [weak self] r in
            guard let self else { return }
            switch r {
            case .success(let text):
                self.rulesWindow.show(title: "SplitR — pf rules", text: text) { [weak self] done in
                    self?.api.rules { r2 in
                        if case .success(let t) = r2 { done(t) }
                    }
                }
            case .failure(let e):
                self.fail(e)
            }
        }
    }

    private func showLog() {
        let path = logPath()
        // Журнал берём у демона (GET /log), а не читаем файл: он пишется от root,
        // и права на файл перестают быть нашей заботой.
        fetchLog { [weak self] text in
            self?.logWindow.show(title: "SplitR — log (\(path))", text: text) { done in
                self?.fetchLog(done)
            }
        }
    }

    private func fetchLog(_ done: @escaping (String) -> Void) {
        api.log(tail: 500) { r in
            switch r {
            case .success(let text): done(text)
            case .failure(let e): done("Could not read the log: " + (e.errorDescription ?? "?"))
            }
        }
    }

    /// Живой поток отброшенных пакетов: единственный способ увидеть глазами,
    /// что защита действительно что-то режет прямо сейчас.
    private func showBlocked() {
        let host = config?.httpAddr ?? AppDelegate.apiAddress
        blockedWindow.onClose = { [weak self] in self?.blockedStream.stop() }
        blockedWindow.show(title: "SplitR — blocked packets",
                           text: "Waiting for packets… (nothing here means nothing is being blocked right now)")
        blockedStream.onLine = { [weak self] line in self?.blockedWindow.append(line) }
        blockedStream.onError = { [weak self] msg in self?.blockedWindow.append("⚠︎ " + msg) }
        blockedStream.start(host: host)
    }

    private func openConfig() {
        let path = status?.configPath.isEmpty == false
            ? status!.configPath
            : "/usr/local/etc/splitr/config.yaml"
        // Конфиг лежит под root, поэтому открываем в редакторе только на чтение;
        // сохранить правку получится лишь через sudo — об этом честно пишем.
        NSWorkspace.shared.open(URL(fileURLWithPath: path))
    }

    // MARK: - обновление

    /// Текст окна прогресса храним отдельно от окна: окно можно закрыть
    /// посреди сборки и открыть снова, и вывод make при этом теряться не должен.
    private var updateLog = ""

    private func checkUpdate() {
        guard begin(busy: "checking for updates…") else { return }
        api.updateInfo { [weak self] r in
            guard let self else { return }
            self.busy = nil
            switch r {
            case .success(let up):
                // Кладём ответ прямо в снимок статуса: меню читает обновление
                // оттуда, и два источника правды здесь были бы лишними.
                self.status?.update = up
                self.lastAction = up.canUpdate
                    ? "update available: \(up.latest)"
                    : (up.reason.isEmpty ? "no update available" : up.reason)
                self.render()
            case .failure(let e):
                // Старый демон про обновления не знает — это не ошибка,
                // о которой стоит кричать модалкой.
                self.lastAction = e.isNotFound ? "this daemon cannot check for updates" : "error: " + (e.errorDescription ?? "unknown")
            }
        }
    }

    private func startUpdate() {
        guard updater == nil else { return }
        guard let up = status?.update, up.canUpdate else { return }

        var text = """
        SplitR will build \(up.latest) in \(up.repoPath), install the daemon and replace this menu bar app.

        The daemon restarts, so the tunnel is restarted too: if it is up now, traffic goes out unprotected for a few seconds while it comes back. Building takes a few minutes.
        """
        if !up.notes.isEmpty { text += "\n\n" + up.notes }
        guard confirm(title: "Update SplitR to \(up.latest)?", text: text,
                      confirmTitle: "Update", destructive: false) else { return }

        updateLog = "Updating SplitR \(up.installed) → \(up.latest)\nRepository: \(up.repoPath)\n"
        updateWindow.show(title: "SplitR — update", text: updateLog)

        let u = Updater(repoPath: up.repoPath, socketPath: socketPath(),
                        apiHost: config?.httpAddr ?? AppDelegate.apiAddress)
        u.onLog = { [weak self] line in self?.appendUpdateLog(line) }
        u.onStage = { [weak self] stage in
            self?.updateStage = stage
            self?.lastAction = "updating… (\(stage))"
        }
        u.onFinish = { [weak self] error, needsRestart in self?.finishUpdate(error, needsRestart) }
        updater = u
        updateStage = "building"
        lastAction = "updating… (building)"
        u.start()
    }

    private func appendUpdateLog(_ line: String) {
        updateLog += line + "\n"
        updateWindow.append(line)
    }

    private func finishUpdate(_ error: String?, _ needsRestart: Bool) {
        updater = nil
        updateStage = nil
        if let error {
            appendUpdateLog("")
            appendUpdateLog("FAILED: " + error)
            lastAction = "update failed"
            alert(title: "The update did not go through", text: error, style: .warning)
            refresh()
            return
        }
        if needsRestart {
            appendUpdateLog("")
            appendUpdateLog("SplitR quits now and starts again as the new version.")
            lastAction = "update installed"
            // Выходим сразу: бандл заменит отсоединённый скрипт, который ждёт
            // именно нашего выхода, и он же запустит новую копию.
            NSApp.terminate(nil)
            return
        }
        appendUpdateLog("")
        appendUpdateLog("Done. The daemon runs the new version.")
        lastAction = "update installed"
        refresh()
    }

    private func showKickstart() {
        let a = NSAlert()
        a.messageText = "The SplitR daemon is not running"
        a.informativeText = "Start it from a terminal:\n\n\(kickstartCommand)\n\nIf the daemon is not installed yet: sudo splitr install"
        a.addButton(withTitle: "Copy command")
        a.addButton(withTitle: "Close")
        NSApp.activate(ignoringOtherApps: true)
        if a.runModal() == .alertFirstButtonReturn { copyKickstart() }
    }

    private func copyKickstart() {
        NSPasteboard.general.clearContents()
        NSPasteboard.general.setString(kickstartCommand, forType: .string)
    }

    // MARK: - вспомогательное

    private func handle(_ r: Result<DaemonStatus, APIError>, success: String) {
        busy = nil
        switch r {
        case .success(let st):
            status = st
            lastError = nil
            lastAction = success
            // Ответ на любую команду — это ещё и свежий снимок туннеля.
            // Не проверить по нему начатую операцию значит подождать
            // подтверждения лишний тик, имея его уже на руках.
            checkPending(against: st)
            render()
        case .failure(let e):
            fail(e)
        }
        refresh()
    }

    private func fail(_ e: APIError) {
        busy = nil
        lastAction = "error: " + (e.errorDescription ?? "unknown")
        // «Демон недоступен» и так видно по иконке — модалку показываем только
        // на содержательных ошибках демона, иначе выключенный демон завалит
        // экран алертами.
        if !e.isUnreachable {
            alert(title: "SplitR: the command did not go through",
                  text: e.errorDescription ?? "unknown error", style: .warning)
        }
    }

    /// Открыто ли меню прямо сейчас. Пока оно открыто, AppKit ведёт свой цикл
    /// отслеживания, и модальное окно поверх него в лучшем случае появляется
    /// за меню, а в худшем подвешивает и то, и другое. Опрос идёт в режиме
    /// .common, то есть отчёт об ошибке вполне может прийти ровно тогда, когда
    /// человек держит меню открытым, — как раз наблюдая за подключением.
    private var menuIsOpen = false
    /// Отложенные показы: то, что пришло при открытом меню.
    private var queuedAlerts: [() -> Void] = []

    func menuWillOpen(_ menu: NSMenu) { menuIsOpen = true }

    func menuDidClose(_ menu: NSMenu) {
        menuIsOpen = false
        let queued = queuedAlerts
        queuedAlerts = []
        // Не сразу: меню ещё сворачивается, и окно, показанное в этот же
        // оборот цикла, встаёт под ним.
        DispatchQueue.main.async { queued.forEach { $0() } }
    }

    /// Показывает окно, а при открытом меню — откладывает до его закрытия.
    private func whenMenuClosed(_ show: @escaping () -> Void) -> Bool {
        guard menuIsOpen else { return false }
        queuedAlerts.append(show)
        return true
    }

    private func alert(title: String, text: String, style: NSAlert.Style) {
        if whenMenuClosed({ [weak self] in self?.alert(title: title, text: text, style: style) }) { return }
        let a = NSAlert()
        a.alertStyle = style
        a.messageText = title
        a.informativeText = text
        a.addButton(withTitle: "OK")
        NSApp.activate(ignoringOtherApps: true)
        a.runModal()
    }

    private func confirm(title: String, text: String, confirmTitle: String, destructive: Bool) -> Bool {
        let a = NSAlert()
        a.alertStyle = destructive ? .critical : .warning
        a.messageText = title
        a.informativeText = text
        let yes = a.addButton(withTitle: confirmTitle)
        if destructive, #available(macOS 11.0, *) { yes.hasDestructiveAction = true }
        a.addButton(withTitle: "Cancel")
        NSApp.activate(ignoringOtherApps: true)
        return a.runModal() == .alertFirstButtonReturn
    }

    private func logPath() -> String {
        if let p = status?.logFile, !p.isEmpty { return p }
        if let p = config?.logFile, !p.isEmpty { return p }
        return "/usr/local/var/log/splitr.log"
    }

    /// Путь к управляющему сокету: из конфига, если он уже прочитан.
    private func socketPath() -> String {
        config?.socketPath ?? SocketClient.defaultPath
    }
}
