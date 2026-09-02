import AppKit

// Точка входа. NSApplicationMain здесь не годится: у приложения нет ни storyboard,
// ни nib — только строка меню, поэтому цикл событий поднимаем руками.

// Служебные режимы: используются установщиком, чтобы не дублировать логику
// автозапуска в shell. Обработать их надо до NSApplication.shared — регистрация
// объекта входа не требует ни цикла событий, ни строки меню.
let args = CommandLine.arguments.dropFirst()
if args.contains("--register-login-item") {
    do {
        try LoginItem.register()
        print("login item registered (SMAppService): \(LoginItem.state.rawValue)")
        exit(0)
    } catch {
        FileHandle.standardError.write(Data("SMAppService failed: \(error.localizedDescription)\n".utf8))
        exit(1)
    }
}
if args.contains("--unregister-login-item") {
    try? LoginItem.unregister()
    print("login item removed: \(LoginItem.state.rawValue)")
    exit(0)
}

// .accessory дублирует LSUIElement из Info.plist: если бинарь запустили не из
// бандла (например, при отладке), плист не читается, и без этой строки в доке
// появилась бы лишняя иконка. Ставить политику надо до app.run(), иначе строка
// меню приложения успевает встать в бар и мигнуть.
let app = NSApplication.shared
app.setActivationPolicy(.accessory)
let delegate = AppDelegate()
app.delegate = delegate
app.run()
