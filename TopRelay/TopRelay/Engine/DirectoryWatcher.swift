// Calls back when files in a directory change. state.json is replaced atomically (written to a temp file, then
// renamed), which changes the directory, not the old file, so we watch the directory. A slow timer backs it up in
// case the directory doesn't exist yet or is recreated.

import Foundation

@MainActor
final class DirectoryWatcher {
    private let directory: URL
    private let onChange: @MainActor () -> Void
    private var source: DispatchSourceFileSystemObject?
    private var timer: Timer?

    init(directory: URL, onChange: @escaping @MainActor () -> Void) {
        self.directory = directory
        self.onChange = onChange
    }

    func start() {
        stop()
        attach()
        timer = Timer.scheduledTimer(withTimeInterval: 5, repeats: true) { [weak self] _ in
            MainActor.assumeIsolated {
                guard let self else { return }
                if self.source == nil { self.attach() }
                self.onChange()
            }
        }
    }

    func stop() {
        source?.cancel()
        source = nil
        timer?.invalidate()
        timer = nil
    }

    private func attach() {
        let fd = open(directory.path, O_EVTONLY)
        guard fd >= 0 else { return }
        let source = DispatchSource.makeFileSystemObjectSource(fileDescriptor: fd, eventMask: [.write, .rename, .delete], queue: .main)
        source.setEventHandler { [weak self] in
            MainActor.assumeIsolated {
                guard let self else { return }
                if source.data.contains(.delete) || source.data.contains(.rename) {
                    // The directory itself went away or moved: reattach on the next timer tick.
                    self.source?.cancel()
                    self.source = nil
                }
                self.onChange()
            }
        }
        source.setCancelHandler { close(fd) }
        source.resume()
        self.source = source
    }
}
