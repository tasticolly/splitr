import AppKit

/// Окно моноширинного текста для правил pf и логов.
///
/// NSAlert для этого не годится: правила и лог — это десятки строк, которые
/// хочется листать и копировать. Одно окно переиспользуется, чтобы повторные
/// клики по «Показать правила» не плодили копии на весь экран.
final class TextWindow: NSObject, NSWindowDelegate {
    private var window: NSWindow?
    private var textView: NSTextView?
    /// Чем наполнять окно при обновлении — задаётся при показе, используется кнопкой «Обновить».
    private var reload: ((@escaping (String) -> Void) -> Void)?
    /// Вызывается при закрытии окна: живой поток надо останавливать,
    /// иначе tcpdump в демоне продолжит работать в никуда.
    var onClose: (() -> Void)?
    private var refreshButton: NSButton?
    private var applyButton: NSButton?
    private var statusLabel: NSTextField?
    /// Обработчик кнопки «Применить»: получает текст и колбэк, которым
    /// сообщает результат. Окно само не закрывается — при ошибке валидации
    /// человек должен видеть свой текст и исправлять его на месте.
    private var apply: ((String, @escaping (Bool, String) -> Void) -> Void)?

    func show(title: String, text: String, reload: ((@escaping (String) -> Void) -> Void)? = nil) {
        self.reload = reload
        self.apply = nil
        // Кнопки прячем после создания окна, а не до: при первом показе их
        // ещё не существует, и «Обновить» оставалась висеть в окне, которому
        // нечем обновляться (окно прогресса обновления, например).
        let win = window ?? makeWindow()
        refreshButton?.isHidden = (reload == nil)
        applyButton?.isHidden = true
        textView?.isEditable = false
        setStatus("", isError: false)
        win.title = title
        set(text: text)
        // LSUIElement-приложение не активируется само по клику в меню,
        // поэтому без явной активации окно откроется под чужими окнами.
        NSApp.activate(ignoringOtherApps: true)
        win.makeKeyAndOrderFront(nil)
    }

    /// Редактируемый вариант: тот же текст, но с правкой и кнопкой «Применить».
    /// Отдельного класса не завёл — окно, шрифт, скролл и жизненный цикл здесь
    /// ровно те же, различие только в двух контролах.
    func showEditor(title: String, text: String,
                    apply: @escaping (String, @escaping (Bool, String) -> Void) -> Void) {
        self.reload = nil
        self.apply = apply
        let win = window ?? makeWindow()
        win.title = title
        set(text: text)
        textView?.isEditable = true
        textView?.scrollToBeginningOfDocument(nil)
        refreshButton?.isHidden = true
        applyButton?.isHidden = false
        setStatus("Edit, then press Apply. The daemon validates the configuration and reloads it.", isError: false)
        NSApp.activate(ignoringOtherApps: true)
        win.makeKeyAndOrderFront(nil)
    }

    private func makeWindow() -> NSWindow {
        let win = NSWindow(
            contentRect: NSRect(x: 0, y: 0, width: 760, height: 520),
            styleMask: [.titled, .closable, .resizable, .miniaturizable],
            backing: .buffered, defer: false)
        win.isReleasedWhenClosed = false // иначе повторный показ обратится к освобождённому окну
        win.center()
        win.delegate = self

        let scroll = NSScrollView()
        scroll.hasVerticalScroller = true
        scroll.hasHorizontalScroller = true
        scroll.autohidesScrollers = false
        scroll.translatesAutoresizingMaskIntoConstraints = false

        let tv = NSTextView()
        tv.isEditable = false
        tv.isRichText = false
        tv.font = NSFont.monospacedSystemFont(ofSize: 11, weight: .regular)
        tv.textContainerInset = NSSize(width: 8, height: 8)
        tv.isHorizontallyResizable = true
        tv.textContainer?.widthTracksTextView = false
        let huge = CGFloat.greatestFiniteMagnitude
        tv.textContainer?.containerSize = NSSize(width: huge, height: huge)
        tv.maxSize = NSSize(width: huge, height: huge)
        scroll.documentView = tv
        textView = tv

        let refresh = NSButton(title: "Refresh", target: self, action: #selector(refreshClicked))
        refresh.translatesAutoresizingMaskIntoConstraints = false
        refreshButton = refresh

        let applyBtn = NSButton(title: "Apply", target: self, action: #selector(applyClicked))
        applyBtn.translatesAutoresizingMaskIntoConstraints = false
        applyBtn.keyEquivalent = "\r"
        applyBtn.isHidden = true
        applyButton = applyBtn

        let label = NSTextField(labelWithString: "")
        label.translatesAutoresizingMaskIntoConstraints = false
        label.lineBreakMode = .byWordWrapping
        label.maximumNumberOfLines = 4
        label.font = NSFont.systemFont(ofSize: 11)
        statusLabel = label

        let content = NSView()
        content.addSubview(scroll)
        content.addSubview(refresh)
        content.addSubview(applyBtn)
        content.addSubview(label)
        NSLayoutConstraint.activate([
            scroll.topAnchor.constraint(equalTo: content.topAnchor),
            scroll.leadingAnchor.constraint(equalTo: content.leadingAnchor),
            scroll.trailingAnchor.constraint(equalTo: content.trailingAnchor),
            refresh.topAnchor.constraint(equalTo: scroll.bottomAnchor, constant: 8),
            refresh.trailingAnchor.constraint(equalTo: content.trailingAnchor, constant: -12),
            refresh.bottomAnchor.constraint(equalTo: content.bottomAnchor, constant: -10),
            applyBtn.trailingAnchor.constraint(equalTo: content.trailingAnchor, constant: -12),
            applyBtn.centerYAnchor.constraint(equalTo: refresh.centerYAnchor),
            label.leadingAnchor.constraint(equalTo: content.leadingAnchor, constant: 12),
            label.trailingAnchor.constraint(lessThanOrEqualTo: applyBtn.leadingAnchor, constant: -12),
            label.centerYAnchor.constraint(equalTo: refresh.centerYAnchor),
        ])
        win.contentView = content
        window = win
        return win
    }

    private func set(text: String) {
        textView?.string = text.isEmpty ? "(empty)" : text
        textView?.scrollToEndOfDocument(nil)
    }

    /// Дописывает строку в конец — для живого потока, где перерисовывать
    /// весь текст на каждый пакет слишком расточительно.
    func append(_ line: String) {
        guard let tv = textView else { return }
        tv.textStorage?.append(NSAttributedString(
            string: line + "\n",
            attributes: [.font: NSFont.monospacedSystemFont(ofSize: 11, weight: .regular),
                         .foregroundColor: NSColor.labelColor]))
        tv.scrollToEndOfDocument(nil)
    }

    func windowWillClose(_ notification: Notification) {
        onClose?()
    }

    private func setStatus(_ text: String, isError: Bool) {
        statusLabel?.stringValue = text
        statusLabel?.textColor = isError ? .systemRed : .secondaryLabelColor
    }

    @objc private func applyClicked() {
        guard let apply, let text = textView?.string else { return }
        applyButton?.isEnabled = false
        setStatus("Applying…", isError: false)
        apply(text) { [weak self] ok, message in
            self?.applyButton?.isEnabled = true
            self?.setStatus(message, isError: !ok)
        }
    }

    @objc private func refreshClicked() {
        reload? { [weak self] text in self?.set(text: text) }
    }
}
