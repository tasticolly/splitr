import Foundation

/// Живой поток отброшенных пакетов (GET /blocked, Server-Sent Events).
///
/// URLSession умеет отдавать тело по кускам только через делегат, поэтому
/// у потока отдельная сессия: у общей APIClient стоит таймаут в несколько
/// секунд, и он бы рвал бесконечный поток каждые пару секунд.
final class BlockedStream: NSObject, URLSessionDataDelegate {
    private var session: URLSession?
    private var task: URLSessionDataTask?
    /// Хвост недочитанной строки: SSE приходит произвольными кусками,
    /// и последний кусок почти всегда обрывается на середине строки.
    private var tail = Data()

    var onLine: ((String) -> Void)?
    var onError: ((String) -> Void)?

    func start(host: String) {
        stop()
        let cfg = URLSessionConfiguration.ephemeral
        cfg.timeoutIntervalForRequest = .infinity
        cfg.timeoutIntervalForResource = .infinity
        cfg.connectionProxyDictionary = [:]
        let s = URLSession(configuration: cfg, delegate: self, delegateQueue: .main)
        session = s
        var req = URLRequest(url: URL(string: "http://\(host)/blocked")!)
        req.setValue("text/event-stream", forHTTPHeaderField: "Accept")
        task = s.dataTask(with: req)
        task?.resume()
    }

    func stop() {
        task?.cancel()
        task = nil
        // finishTasksAndInvalidate, а не invalidateAndCancel: делегат должен
        // успеть отпустить ссылку на себя, иначе сессия удержит объект навсегда.
        session?.finishTasksAndInvalidate()
        session = nil
        tail.removeAll()
    }

    func urlSession(_ session: URLSession, dataTask: URLSessionDataTask,
                    didReceive response: URLResponse,
                    completionHandler: @escaping (URLSession.ResponseDisposition) -> Void) {
        guard let http = response as? HTTPURLResponse, http.statusCode < 400 else {
            let code = (response as? HTTPURLResponse)?.statusCode ?? 0
            // 412 — самый частый случай: protection.log выключен в конфиге.
            onError?(code == 412
                     ? "Packet logging is off. Turn on protection.log in the configuration and reload it."
                     : "The daemon answered HTTP \(code)")
            completionHandler(.cancel)
            return
        }
        completionHandler(.allow)
    }

    func urlSession(_ session: URLSession, dataTask: URLSessionDataTask, didReceive data: Data) {
        tail.append(data)
        while let nl = tail.firstIndex(of: 0x0A) {
            let line = String(decoding: tail[tail.startIndex..<nl], as: UTF8.self)
            tail.removeSubrange(tail.startIndex...nl)
            if line.hasPrefix("data: ") {
                onLine?(String(line.dropFirst(6)))
            }
        }
    }

    func urlSession(_ session: URLSession, task: URLSessionTask, didCompleteWithError error: Error?) {
        if let error, (error as NSError).code != NSURLErrorCancelled {
            onError?(error.localizedDescription)
        }
    }
}
