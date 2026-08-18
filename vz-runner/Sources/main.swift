import Foundation
import Virtualization
import Darwin
import AppKit

// MARK: - Command Line Arguments

struct SharedDir {
    var hostPath: String
    var tag: String
    var readOnly: Bool
}

// Ceilings for the numeric flags. They are deliberately far above anything a
// real host can satisfy -- Virtualization.framework clamps to the machine's
// actual limits later -- and exist only to keep a hostile or fat-fingered
// value out of the arithmetic that builds the configuration.
let maxMemoryMB = 1 << 20   // 1 TiB expressed in MB
let maxCPUCount = 1 << 12

// boundedInt parses a numeric flag, rejecting anything the runner cannot use.
//
// The unchecked `Int(args[i]) ?? default` this replaces let a negative or
// absurd value straight into `UInt64(config.memoryMB) * 1024 * 1024`, where
// Swift's trapping conversion and trapping multiply killed the process with
// SIGTRAP: exit 133, no diagnostic, nothing in the log to explain it. A bad
// argument has to be a sentence on stderr, not a crash.
func boundedInt(_ raw: String, flag: String, min lower: Int, max upper: Int) -> Int? {
    guard let value = Int(raw) else {
        print("VZRunner: \(flag) expects a number, got '\(raw)'", to: &standardError)
        return nil
    }
    guard value >= lower && value <= upper else {
        print("VZRunner: \(flag) must be between \(lower) and \(upper), got \(value)", to: &standardError)
        return nil
    }
    return value
}

// isDatagramSocket reports whether fd is open and is the AF_UNIX datagram
// socket the gateway hands down. getsockopt fails with EBADF for a closed
// descriptor and ENOTSOCK for one that is not a socket, so both mistakes are
// caught before Virtualization.framework raises on them.
func isDatagramSocket(_ fd: Int32) -> Bool {
    var sockType: Int32 = 0
    var length = socklen_t(MemoryLayout<Int32>.size)
    if getsockopt(fd, SOL_SOCKET, SO_TYPE, &sockType, &length) != 0 {
        return false
    }
    return sockType == SOCK_DGRAM
}

struct VMConfig {
    var kernelPath: String = ""
    var initrdPath: String = ""
    var rootfsPath: String = ""
    var cmdline: String = ""
    var memoryMB: Int = 512
    var cpuCount: Int = 1
    var qmpSocket: String = ""
    var agentSockPath: String = ""
    var macAddress: String = ""
    var netFD: Int32 = -1
    // Attach no network device at all. Absence of --net-fd cannot mean this:
    // it already means "use Apple NAT", so a guest asked for no network got
    // the internet. The request has to be said out loud.
    var noNet = false
    var stopGraceSeconds: Int = 10
    var sharedDirs: [SharedDir] = []
    var useDirectBoot: Bool = true  // Try VZLinuxBootLoader first
    var stateDir: String = ""       // checkpoint artifacts live here; empty disables checkpointing
    var restore: Bool = false       // boot by restoring the saved state instead of a cold start
    var gui = false                 // show the guest display in a native macOS window
    var guiTitle = "urunc"          // window title in --gui mode
    var rosetta = false             // share the Rosetta translator so the arm64 guest kernel can run amd64 userspace

    static func parse(_ args: [String]) -> VMConfig? {
        var config = VMConfig()

        var i = 1
        while i < args.count {
            switch args[i] {
            case "--kernel":
                i += 1
                if i < args.count { config.kernelPath = args[i] }
            case "--initrd":
                i += 1
                if i < args.count { config.initrdPath = args[i] }
            case "--rootfs":
                i += 1
                if i < args.count { config.rootfsPath = args[i] }
            case "--cmdline":
                i += 1
                if i < args.count { config.cmdline = args[i] }
            case "--mem":
                i += 1
                if i < args.count {
                    guard let mb = boundedInt(args[i], flag: "--mem", min: 1, max: maxMemoryMB) else { return nil }
                    config.memoryMB = mb
                }
            case "--cpus":
                i += 1
                if i < args.count {
                    guard let cpus = boundedInt(args[i], flag: "--cpus", min: 1, max: maxCPUCount) else { return nil }
                    config.cpuCount = cpus
                }
            case "--qmp":
                i += 1
                if i < args.count { config.qmpSocket = args[i] }
            case "--agent-sock":
                i += 1
                if i < args.count { config.agentSockPath = args[i] }
            case "--mac":
                i += 1
                if i < args.count { config.macAddress = args[i] }
            case "--net-fd":
                i += 1
                if i < args.count {
                    guard let fd = boundedInt(args[i], flag: "--net-fd", min: 0, max: Int(Int32.max)) else { return nil }
                    // Virtualization.framework raises NSInvalidArgumentException
                    // -- an uncatchable SIGABRT from Swift -- when the handle
                    // does not name an open datagram socket. Check it here so a
                    // stale or mistyped fd is a message instead of a crash.
                    guard isDatagramSocket(Int32(fd)) else {
                        print("VZRunner: --net-fd \(fd) is not an open datagram socket "
                              + "(\(String(cString: strerror(errno)))); it must be inherited from the gateway",
                              to: &standardError)
                        return nil
                    }
                    config.netFD = Int32(fd)
                }
            case "--no-net":
                config.noNet = true
            case "--stop-grace":
                i += 1
                if i < args.count {
                    guard let grace = boundedInt(args[i], flag: "--stop-grace", min: 0, max: 86_400) else { return nil }
                    config.stopGraceSeconds = grace
                }
            case "--efi":
                config.useDirectBoot = false
            case "--state-dir":
                i += 1
                if i < args.count { config.stateDir = args[i] }
            case "--restore":
                config.restore = true
            case "--gui":
                config.gui = true
            case "--gui-title":
                i += 1
                if i < args.count { config.guiTitle = args[i] }
            case "--rosetta":
                config.rosetta = true
            case "--share", "--share-ro":
                // Format: --share <path> <tag>, or --share-ro for a share the
                // guest must not be able to write to. Path and tag are separate
                // arguments to avoid colon-splitting issues, and the mode is a
                // separate flag rather than a third argument so the shape stays
                // fixed. Virtualization.framework enforces the read-only bit on
                // the device itself, so no guest-side mount option is trusted.
                let readOnly = (args[i] == "--share-ro")
                i += 1
                guard i < args.count else { break }
                let hostPath = args[i]
                i += 1
                guard i < args.count else { break }
                let tag = args[i]
                config.sharedDirs.append(SharedDir(hostPath: hostPath, tag: tag, readOnly: readOnly))
            default:
                break
            }
            i += 1
        }

        guard !config.kernelPath.isEmpty else {
            return nil
        }

        if config.restore && config.stateDir.isEmpty {
            print("VZRunner: --restore requires --state-dir", to: &standardError)
            return nil
        }

        return config
    }

    // Checkpoint artifact layout inside stateDir. latest.json is written
    // last and acts as the completion marker the CLI polls for.
    var machineIDPath: String { stateDir + "/machine-id" }
    var vmStatePath: String { stateDir + "/vm.vzstate" }
    var diskStatePath: String { stateDir + "/rootfs.img" }
    var manifestPath: String { stateDir + "/latest.json" }
}

// MARK: - Shell Helper

func runShellQuietly(_ executable: String, _ arguments: [String]) throws -> Int32 {
    let process = Process()
    process.executableURL = URL(fileURLWithPath: executable)
    process.arguments = arguments
    process.standardOutput = FileHandle.nullDevice
    process.standardError = FileHandle.nullDevice
    try process.run()
    process.waitUntilExit()
    return process.terminationStatus
}

func runShellCapture(_ executable: String, _ arguments: [String]) throws -> (Int32, String) {
    let pipe = Pipe()
    let process = Process()
    process.executableURL = URL(fileURLWithPath: executable)
    process.arguments = arguments
    process.standardOutput = pipe
    process.standardError = FileHandle.nullDevice
    try process.run()
    process.waitUntilExit()
    let data = pipe.fileHandleForReading.readDataToEndOfFile()
    return (process.terminationStatus, String(data: data, encoding: .utf8) ?? "")
}

// MARK: - EFI Boot Disk Builder

func createEFIBootDisk(kernelPath: String, initrdPath: String?, cmdline: String) throws -> String {
    let diskPath = "/tmp/vz-runner-efi-boot.raw"
    let dmgPath = "/tmp/vz-runner-efi-boot.dmg"

    try? FileManager.default.removeItem(atPath: dmgPath)
    try? FileManager.default.removeItem(atPath: diskPath)

    let status = try runShellQuietly("/usr/bin/hdiutil",
        ["create", "-size", "128m", "-fs", "FAT32", "-volname", "VZBOOT",
         "-type", "UDIF", "-o", dmgPath.replacingOccurrences(of: ".dmg", with: "")])
    guard status == 0 else {
        throw NSError(domain: "VZRunner", code: 1, userInfo: [NSLocalizedDescriptionKey: "Failed to create boot disk image"])
    }

    // Attach DMG
    let (_, attachOutput) = try runShellCapture("/usr/bin/hdiutil", ["attach", dmgPath])
    guard let mountPoint = attachOutput.split(separator: "\n").last?.split(separator: "\t").last.map(String.init) else {
        throw NSError(domain: "VZRunner", code: 2, userInfo: [NSLocalizedDescriptionKey: "Failed to mount boot disk"])
    }
    let diskDevice = String(attachOutput.split(separator: "\n").first?.split(separator: " ").first ?? "")

    // Create EFI directory structure
    let efiBootDir = "\(mountPoint)/EFI/BOOT"
    try FileManager.default.createDirectory(atPath: efiBootDir, withIntermediateDirectories: true)

    // Copy kernel as EFI boot binary
    try FileManager.default.copyItem(atPath: kernelPath, toPath: "\(efiBootDir)/BOOTAA64.EFI")

    // Copy initrd if provided
    if let initrdPath = initrdPath, !initrdPath.isEmpty {
        try FileManager.default.copyItem(atPath: initrdPath, toPath: "\(mountPoint)/initrd.img")
    }

    // Detach
    _ = try runShellQuietly("/usr/bin/hdiutil", ["detach", diskDevice])

    // Convert to raw
    try? FileManager.default.removeItem(atPath: diskPath)
    _ = try runShellQuietly("/usr/bin/hdiutil",
        ["convert", dmgPath, "-format", "UDTO", "-o", "/tmp/vz-runner-efi-boot"])

    let cdrPath = "/tmp/vz-runner-efi-boot.cdr"
    if FileManager.default.fileExists(atPath: cdrPath) {
        try? FileManager.default.removeItem(atPath: diskPath)
        try FileManager.default.moveItem(atPath: cdrPath, toPath: diskPath)
    }

    try? FileManager.default.removeItem(atPath: dmgPath)
    return diskPath
}

// MARK: - Guest Console Filtering

// writeAllToStdout writes every byte or gives up on a hard error, without the
// exceptions FileHandle.write raises on a closed console.
func writeAllToStdout(_ bytes: [UInt8]) {
    guard !bytes.isEmpty else { return }
    bytes.withUnsafeBufferPointer { buffer in
        var offset = 0
        while offset < buffer.count {
            let written = write(STDOUT_FILENO, buffer.baseAddress! + offset, buffer.count - offset)
            if written <= 0 {
                if errno == EINTR { continue }
                return
            }
            offset += written
        }
    }
}

/// TerminalSanitizer neutralises the escape sequences a guest can use to make
/// the operator's terminal type on its behalf.
///
/// A terminal is not a display: several sequences ask it a question, and the
/// answer is written into the tty input buffer, where the next reader picks it
/// up. In foreground `hull run` the guest's serial console is wired straight to
/// that terminal, so a guest emitting OSC 52 gets the host clipboard, and a
/// guest that sets the window title (OSC 2) and then asks for it back (CSI 21 t)
/// gets a line of text of its choosing delivered as if the operator had typed
/// it.
///
/// The policy matches hull's `exec` filter byte for byte, and is deliberately
/// narrow so a TUI still works: colour, cursor movement, scroll regions, mouse
/// tracking, bracketed paste and the alternate screen pass through. Dropped
/// are OSC 52 and OSC 1337, palette queries (OSC 4/5/10-21 carrying "?"),
/// CSI c / CSI n / CSI > q / CSI $ p / CSI x identity and status queries, the
/// CSI t window operations other than the 22 and 23 title-stack pair, ESC Z,
/// every DCS/SOS/PM/APC string (tmux and screen forward those verbatim to the
/// outer terminal, which would bypass this filter), and C1 controls appearing
/// where a UTF-8 lead byte belongs -- 0x9B, 0x9D and 0x90 are the 8-bit
/// spelling of CSI, OSC and DCS.
struct TerminalSanitizer {
    // Bytes held back because an escape sequence or a rune was split across
    // writes. Bounded: an unterminated string sequence must not buffer the
    // whole session or stall its output waiting for a terminator that is never
    // coming.
    private var held: [UInt8] = []
    private static let maxHeld = 4096

    mutating func filter(_ bytes: [UInt8]) -> [UInt8] {
        var buf = held
        buf.append(contentsOf: bytes)
        var out: [UInt8] = []
        out.reserveCapacity(buf.count)

        var i = 0
        while i < buf.count {
            let c = buf[i]
            if c == 0x1b {
                guard let n = TerminalSanitizer.escapeLength(buf, from: i) else { break }
                let seq = Array(buf[i..<(i + n)])
                if TerminalSanitizer.escapeIsSafe(seq) {
                    out.append(contentsOf: seq)
                }
                i += n
            } else if c >= 0x80 && c < 0xc0 {
                // Never a valid UTF-8 lead byte: a C1 control or a stray
                // continuation byte. A C1 *introducer* starts a sequence, and
                // dropping just the introducer would leave its parameters to
                // spill onto the screen as text, so the whole sequence goes --
                // nothing legitimate emits 8-bit controls to a UTF-8 terminal.
                if let equivalent = TerminalSanitizer.c1Introducer(c) {
                    // Measured in place. Copying "ESC <equiv> <rest of buffer>"
                    // for every introducer made this quadratic in the frame
                    // size, and frames are up to a mebibyte: a guest printing
                    // 0x9b pegged a core for as long as it cared to. The bytes
                    // are never needed -- 8-bit controls are dropped whatever
                    // they say -- so only the length matters.
                    guard let n = TerminalSanitizer.escapeLengthAfterIntroducer(
                        equivalent, buf, from: i + 1) else { break }
                    i += n
                } else {
                    i += 1
                }
            } else if c >= 0xc0 {
                let n = TerminalSanitizer.runeLength(c)
                if i + n > buf.count { break }
                out.append(contentsOf: buf[i..<(i + n)])
                i += n
            } else {
                out.append(c)
                i += 1
            }
        }

        held = Array(buf[i...])
        if held.count > TerminalSanitizer.maxHeld { held = [] }
        return out
    }

    // c1Introducer maps an 8-bit C1 control to the byte that follows ESC in
    // its 7-bit spelling, so one parser handles both forms.
    static func c1Introducer(_ c: UInt8) -> UInt8? {
        switch c {
        case 0x90: return UInt8(ascii: "P")  // DCS
        case 0x98: return UInt8(ascii: "X")  // SOS
        case 0x9b: return UInt8(ascii: "[")  // CSI
        case 0x9d: return UInt8(ascii: "]")  // OSC
        case 0x9e: return UInt8(ascii: "^")  // PM
        case 0x9f: return UInt8(ascii: "_")  // APC
        default: return nil
        }
    }

    private static func runeLength(_ lead: UInt8) -> Int {
        if lead >= 0xf0 { return 4 }
        if lead >= 0xe0 { return 3 }
        if lead >= 0xc0 { return 2 }
        return 1
    }

    // escapeLength measures the sequence starting at ESC, returning nil while
    // it is still incomplete.

    /// Measures a sequence whose 8-bit introducer has already been consumed,
    /// without copying the remainder. `intro` is the byte that would follow ESC
    /// in the 7-bit spelling; `start` indexes the first byte after the
    /// introducer. The count includes the introducer.
    private static func escapeLengthAfterIntroducer(
        _ intro: UInt8, _ b: [UInt8], from start: Int
    ) -> Int? {
        switch intro {
        case UInt8(ascii: "["):
            var i = start
            while i < b.count && b[i] >= 0x30 && b[i] <= 0x3f { i += 1 }
            while i < b.count && b[i] >= 0x20 && b[i] <= 0x2f { i += 1 }
            guard i < b.count else { return nil }
            if b[i] == 0x1b { return i - start + 1 }
            return i - start + 2
        case UInt8(ascii: "]"), UInt8(ascii: "P"), UInt8(ascii: "X"),
             UInt8(ascii: "^"), UInt8(ascii: "_"):
            var i = start
            while i < b.count {
                if b[i] == 0x07 || b[i] == 0x9c { return i - start + 2 }
                if b[i] == 0x1b {
                    guard i + 1 < b.count else { return nil }
                    if b[i + 1] == UInt8(ascii: "\\") { return i - start + 3 }
                    return i - start + 1
                }
                i += 1
            }
            return nil
        default:
            return 1
        }
    }

    private static func escapeLength(_ b: [UInt8], from start: Int) -> Int? {
        guard start + 1 < b.count else { return nil }
        let kind = b[start + 1]
        switch kind {
        case UInt8(ascii: "["):
            // CSI: parameter bytes, intermediate bytes, then one final byte.
            var i = start + 2
            while i < b.count && b[i] >= 0x30 && b[i] <= 0x3f { i += 1 }
            while i < b.count && b[i] >= 0x20 && b[i] <= 0x2f { i += 1 }
            guard i < b.count else { return nil }
            if b[i] == 0x1b {
                // ESC is not a final byte. A terminal treats it as an
                // "anywhere" transition and abandons this sequence, so counting
                // it as the final byte let "ESC [ ESC ] 52;c;... BEL" through:
                // the CSI measured as complete and harmless, and the OSC behind
                // it was then read as ordinary text. Stop before the ESC; the
                // fragment is refused and the stream resynchronises on it.
                return i - start
            }
            return i + 1 - start
        case UInt8(ascii: "]"), UInt8(ascii: "P"), UInt8(ascii: "X"),
             UInt8(ascii: "^"), UInt8(ascii: "_"):
            return stringSequenceLength(b, from: start)
        default:
            if kind == 0x1b {
                // ESC cancels whatever it interrupts, which is also how a tmux
                // passthrough smuggles a nested sequence past a naive scanner.
                // Consume this byte alone so the next ESC is judged on its own.
                return 1
            }
            if kind >= 0x20 && kind <= 0x2f {
                var i = start + 2
                while i < b.count && b[i] >= 0x20 && b[i] <= 0x2f { i += 1 }
                guard i < b.count else { return nil }
                if b[i] == 0x1b { return i - start }  // as in the CSI case above
                return i + 1 - start
            }
            return 2
        }
    }

    // stringSequenceLength measures an ST- or BEL-terminated string. An ESC
    // that does not begin ST ends the measurement, so an unterminated string
    // cannot swallow everything after it.
    private static func stringSequenceLength(_ b: [UInt8], from start: Int) -> Int? {
        var i = start + 2
        while i < b.count {
            if b[i] == 0x07 || b[i] == 0x9c { return i + 1 - start }
            if b[i] == 0x1b {
                guard i + 1 < b.count else { return nil }
                if b[i + 1] == UInt8(ascii: "\\") { return i + 2 - start }
                return i - start
            }
            i += 1
        }
        return nil
    }

    static func escapeIsSafe(_ seq: [UInt8]) -> Bool {
        guard seq.count >= 2 else { return false }
        // A sequence cut short at an ESC is a fragment, and a fragment must
        // never be emitted: the terminal does not stop where our scanner
        // stopped. Given "ESC [ 1;2" it keeps consuming, so the next ordinary
        // character passed through becomes its final byte -- "hello" turns it
        // into ESC[1;2h and sets a mode. Requiring a real final byte is what
        // closes it; testing for a trailing ESC does not, because the fragment
        // does not include the ESC.
        if seq.last == 0x1b { return false }
        // ESC followed by an 8-bit C1 introducer. escapeLength treats
        // "ESC <byte>" as a complete two-byte sequence for anything outside its
        // known set, and 0x90/0x9b/0x9d are outside it -- so the pair was
        // emitted whole and the terminal took the C1 as an anywhere-transition
        // into CSI/OSC state, making every following byte the filter passed as
        // ordinary text into the sequence body.
        //
        // The "we are in UTF-8, C1 bytes are not controls" argument does not
        // hold, because this same filter passed ESC % @, which leaves UTF-8.
        if seq[1] >= 0x80 { return false }
        if seq[1] == UInt8(ascii: "%") { return false }
        // A CSI must end in a real final byte, so a fragment is never emitted.
        if seq[1] == UInt8(ascii: "[") {
            guard let last = seq.last, last >= 0x40, last <= 0x7e else { return false }
        }
        // ESC <intermediate> <final> is a different family with a different
        // final range: 0x30-0x7e. Applying the CSI range to it dropped ESC ( 0,
        // ncurses' box-drawing charset, so every TUI border rendered as the
        // literal letters lqqqk.
        if seq[1] >= 0x20 && seq[1] <= 0x2f {
            guard let last = seq.last, last >= 0x30, last <= 0x7e else { return false }
        }
        switch seq[1] {
        case UInt8(ascii: "["):
            return csiIsSafe(seq)
        case UInt8(ascii: "]"):
            return oscIsSafe(seq)
        case UInt8(ascii: "P"), UInt8(ascii: "X"), UInt8(ascii: "^"), UInt8(ascii: "_"):
            return false
        case UInt8(ascii: "Z"):
            // DECID answers with a device attributes string.
            return false
        case UInt8(ascii: "\\"):
            // A string terminator with no string in front of it: it draws
            // nothing, and it is what is left over when a smuggled sequence is
            // unpicked.
            return false
        default:
            return true
        }
    }

    private static func csiIsSafe(_ seq: [UInt8]) -> Bool {
        let body = Array(seq[2...])
        guard let final = body.last else { return false }
        var rest = Array(body.dropLast())
        var intermediates: [UInt8] = []
        while let last = rest.last, last >= 0x20, last <= 0x2f {
            intermediates.insert(last, at: 0)
            rest.removeLast()
        }
        let params = String(decoding: rest, as: UTF8.self)
        let inter = String(decoding: intermediates, as: UTF8.self)

        switch final {
        case UInt8(ascii: "c"):        // device attributes
            return false
        case UInt8(ascii: "n"):        // device status, incl. cursor position
            return false
        case UInt8(ascii: "x"):        // DECREQTPARM; DECSACE has an intermediate
            return !inter.isEmpty
        case UInt8(ascii: "q"):        // CSI > q (XTVERSION) reports the terminal
            return !params.hasPrefix(">")
        case UInt8(ascii: "p"):        // CSI ... $ p (DECRQM) reports a mode
            return inter != "$"
        case UInt8(ascii: "t"):        // window ops: 18/19 geometry, 20/21 title
            return params == "22" || params == "23"
                || params.hasPrefix("22;") || params.hasPrefix("23;")
        default:
            return true
        }
    }

    // The OSC codes that set colours and accept "?" as a query.
    private static let queryableOSC: Set<String> = [
        "4", "5", "10", "11", "12", "13", "14", "15", "16", "17", "18", "19", "20", "21",
    ]

    private static func oscIsSafe(_ seq: [UInt8]) -> Bool {
        var body = Array(seq[2...])
        if let last = body.last, last == 0x07 || last == 0x9c {
            body.removeLast()
        } else if body.count >= 2, body[body.count - 2] == 0x1b,
                  body[body.count - 1] == UInt8(ascii: "\\") {
            body.removeLast(2)
        } else {
            // Unterminated: a truncated write this filter could not reassemble,
            // or an attempt to hide what follows inside the string.
            return false
        }

        let fields = String(decoding: body, as: UTF8.self).components(separatedBy: ";")
        // Parsed as a number, because that is how a terminal reads it: "052"
        // and "0052" are OSC 52 to the terminal and different strings to us. A
        // code we cannot parse is refused -- not knowing what it is rules out
        // saying it is safe.
        guard let first = fields.first, let code = Int(first) else { return false }
        if code == 52 || code == 1337 {
            // Clipboard read/write, and iTerm2's file and variable channel.
            return false
        }
        if queryableOSC.contains(String(code)) {
            for field in fields.dropFirst() where field == "?" {
                return false
            }
        }
        return true
    }
}

// MARK: - Shutdown Handling

var gManager: VZManager?

@_silgen_name("signal_handler")
func signalHandler(sig: Int32) {
    print("VZRunner: Received signal \(sig), shutting down gracefully", to: &standardError)
    if let manager = gManager {
        DispatchQueue.main.async {
            manager.shutdown()
        }
    }
}

@_silgen_name("checkpoint_signal_handler")
func checkpointSignalHandler(sig: Int32) {
    if let manager = gManager {
        DispatchQueue.main.async {
            manager.checkpoint()
        }
    }
}

// MARK: - VM Manager

class VZManager: NSObject, VZVirtualMachineDelegate {
    let config: VMConfig
    var vm: VZVirtualMachine?
    var isShuttingDown = false
    var isCheckpointing = false
    var window: NSWindow?  // retained in --gui mode so the window outlives run()
    // Console filtering state: the pipe the guest's serial output is written
    // into, and the sanitizer draining it. Both are retained here because the
    // readability handler runs for the life of the VM.
    private var consolePipe: Pipe?
    private var consoleSanitizer = TerminalSanitizer()
    private let consoleLock = NSLock()

    init(config: VMConfig) {
        self.config = config
        super.init()
    }

    // consoleOutputHandle returns the file handle the guest's serial port
    // writes into. When stdout is a terminal that is a pipe drained by the
    // sanitizer; otherwise it is stdout itself, so nothing stands between the
    // guest and a redirected console log.
    private func consoleOutputHandle() -> FileHandle {
        guard isatty(STDOUT_FILENO) == 1 else {
            return FileHandle.standardOutput
        }
        let pipe = Pipe()
        consolePipe = pipe
        pipe.fileHandleForReading.readabilityHandler = { [weak self] handle in
            let data = handle.availableData
            if data.isEmpty {
                handle.readabilityHandler = nil
                return
            }
            guard let self else { return }
            self.consoleLock.lock()
            let filtered = self.consoleSanitizer.filter([UInt8](data))
            self.consoleLock.unlock()
            writeAllToStdout(filtered)
        }
        return pipe.fileHandleForWriting
    }

    // drainConsole flushes whatever the guest wrote into the console pipe but
    // the reader has not picked up yet. Without it the exit() in the VM
    // delegate would drop the guest's last line, which used to reach the
    // terminal directly.
    func drainConsole() {
        guard let pipe = consolePipe else { return }
        pipe.fileHandleForReading.readabilityHandler = nil
        let fd = pipe.fileHandleForReading.fileDescriptor
        // Non-blocking: the write end stays open in this process, so a blocking
        // read would wait for a guest that has already stopped.
        let flags = fcntl(fd, F_GETFL, 0)
        if flags >= 0 {
            _ = fcntl(fd, F_SETFL, flags | O_NONBLOCK)
        }
        var buffer = [UInt8](repeating: 0, count: 65536)
        while true {
            let n = buffer.withUnsafeMutableBytes { read(fd, $0.baseAddress, $0.count) }
            if n <= 0 {
                if n < 0 && errno == EINTR { continue }
                break
            }
            consoleLock.lock()
            let filtered = consoleSanitizer.filter(Array(buffer[0..<n]))
            consoleLock.unlock()
            writeAllToStdout(filtered)
        }
    }

    func createVM() throws -> VZVirtualMachine {
        let vmConfig = VZVirtualMachineConfiguration()

        // Platform: Generic (required for Linux guests). With a state dir the
        // machine identifier is persisted: restoring a saved state requires
        // the identical identifier, and a stable identity across reruns also
        // keeps the guest's view of the hardware consistent.
        let platform = VZGenericPlatformConfiguration()
        if !config.stateDir.isEmpty,
           let data = try? Data(contentsOf: URL(fileURLWithPath: config.machineIDPath)),
           let saved = VZGenericMachineIdentifier(dataRepresentation: data) {
            platform.machineIdentifier = saved
        } else {
            platform.machineIdentifier = VZGenericMachineIdentifier()
            if !config.stateDir.isEmpty {
                try? FileManager.default.createDirectory(atPath: config.stateDir, withIntermediateDirectories: true)
                try? platform.machineIdentifier.dataRepresentation.write(
                    to: URL(fileURLWithPath: config.machineIDPath))
            }
        }
        vmConfig.platform = platform

        // CPU and Memory — clamp to allowed range
        let maxCPU = VZVirtualMachineConfiguration.maximumAllowedCPUCount
        let minCPU = VZVirtualMachineConfiguration.minimumAllowedCPUCount
        let maxMem = VZVirtualMachineConfiguration.maximumAllowedMemorySize
        let minMem = VZVirtualMachineConfiguration.minimumAllowedMemorySize
        // Belt and braces behind the parse-time bounds: converting and
        // multiplying an out-of-range --mem is what trapped the process with
        // SIGTRAP and no diagnostic, so the arithmetic itself must not be able
        // to trap even if a future caller reaches createVM another way.
        let (memBytes, memOverflowed) = UInt64(clamping: config.memoryMB)
            .multipliedReportingOverflow(by: 1024 * 1024)
        let requestedMem = memOverflowed ? maxMem : memBytes
        vmConfig.cpuCount = max(minCPU, min(config.cpuCount, maxCPU))
        vmConfig.memorySize = max(minMem, min(requestedMem, maxMem))
        print("VZRunner: CPU: \(vmConfig.cpuCount), Memory: \(vmConfig.memorySize / 1024 / 1024)MB", to: &standardError)

        let cmdline = config.cmdline.isEmpty ? "rdinit=/init console=hvc0" : config.cmdline

        if config.useDirectBoot {
            // Direct boot via VZLinuxBootLoader — passes cmdline to kernel
            let kernelURL = URL(fileURLWithPath: config.kernelPath)
            let bootLoader = VZLinuxBootLoader(kernelURL: kernelURL)
            bootLoader.commandLine = cmdline
            if !config.initrdPath.isEmpty {
                bootLoader.initialRamdiskURL = URL(fileURLWithPath: config.initrdPath)
            }
            vmConfig.bootLoader = bootLoader
            print("VZRunner: Using direct boot (VZLinuxBootLoader)", to: &standardError)
            print("VZRunner: Cmdline: \(cmdline)", to: &standardError)
        } else {
            // EFI boot — kernel cmdline must be embedded in kernel
            let efiStorePath = "/tmp/vz-runner-efi-vars"
            let efiStore: VZEFIVariableStore
            if FileManager.default.fileExists(atPath: efiStorePath) {
                efiStore = VZEFIVariableStore(url: URL(fileURLWithPath: efiStorePath))
            } else {
                efiStore = try VZEFIVariableStore(creatingVariableStoreAt: URL(fileURLWithPath: efiStorePath))
            }
            let bootLoader = VZEFIBootLoader()
            bootLoader.variableStore = efiStore
            vmConfig.bootLoader = bootLoader
            print("VZRunner: Using EFI boot", to: &standardError)

            // Build EFI boot disk with kernel + initrd
            let bootDiskPath = try createEFIBootDisk(
                kernelPath: config.kernelPath,
                initrdPath: config.initrdPath.isEmpty ? nil : config.initrdPath,
                cmdline: cmdline
            )
            print("VZRunner: Created EFI boot disk: \(bootDiskPath)", to: &standardError)

            // EFI boot disk
            let bootDiskAttachment = try VZDiskImageStorageDeviceAttachment(
                url: URL(fileURLWithPath: bootDiskPath), readOnly: true)
            vmConfig.storageDevices.append(VZVirtioBlockDeviceConfiguration(attachment: bootDiskAttachment))
        }

        // Additional rootfs disk if provided
        if !config.rootfsPath.isEmpty {
            let rootfsAttachment = try VZDiskImageStorageDeviceAttachment(
                url: URL(fileURLWithPath: config.rootfsPath), readOnly: false)
            vmConfig.storageDevices.append(VZVirtioBlockDeviceConfiguration(attachment: rootfsAttachment))
        }

        // Memory balloon — only without a state dir: Virtualization.framework
        // refuses to save/restore configurations that contain balloon devices,
        // and the runner never drives the balloon anyway.
        if config.stateDir.isEmpty {
            vmConfig.memoryBalloonDevices = [VZVirtioTraditionalMemoryBalloonDeviceConfiguration()]
        }

        // Entropy device
        vmConfig.entropyDevices = [VZVirtioEntropyDeviceConfiguration()]

        // Serial console — stdin goes straight to the guest, but the guest's
        // output only reaches an interactive terminal through the filter (see
        // TerminalSanitizer): in foreground `hull run` this file handle is the
        // operator's own tty, and a guest that emits an OSC 52 or a device
        // status query would otherwise have the terminal type the answer back
        // into the input buffer. A redirected stdout is passed through
        // untouched, so captured console logs are byte-for-byte as before.
        let serialConfig = VZVirtioConsoleDeviceSerialPortConfiguration()
        let serialAttachment = VZFileHandleSerialPortAttachment(
            fileHandleForReading: FileHandle.standardInput,
            fileHandleForWriting: consoleOutputHandle()
        )
        serialConfig.attachment = serialAttachment
        vmConfig.serialPorts = [serialConfig]

        // GUI display path: virtio-gpu scanout + USB HID + a Spice agent
        // console port for clipboard and dynamic-resize hints. The stdio
        // serial console above is kept so boot logs still stream to the
        // terminal. Attached only with --gui; headless config is unchanged.
        if config.gui {
            let graphics = VZVirtioGraphicsDeviceConfiguration()
            graphics.scanouts = [
                VZVirtioGraphicsScanoutConfiguration(widthInPixels: 1280, heightInPixels: 800)
            ]
            vmConfig.graphicsDevices = [graphics]

            vmConfig.keyboards = [VZUSBKeyboardConfiguration()]
            vmConfig.pointingDevices = [VZUSBScreenCoordinatePointingDeviceConfiguration()]

            // Spice vdagent channel: a virtio console port named with the
            // well-known Spice agent port name, backed by VZSpiceAgentPortAttachment.
            let spiceConsole = VZVirtioConsoleDeviceConfiguration()
            let spicePort = VZVirtioConsolePortConfiguration()
            spicePort.name = VZSpiceAgentPortAttachment.spiceAgentPortName
            spicePort.attachment = VZSpiceAgentPortAttachment()
            spiceConsole.ports[0] = spicePort
            vmConfig.consoleDevices = [spiceConsole]

            print("VZRunner: GUI mode: virtio-gpu 1280x800 + USB HID + Spice agent", to: &standardError)
        }

        // Shared directories via virtiofs
        var dirSharingDevices: [VZVirtioFileSystemDeviceConfiguration] = []
        for share in config.sharedDirs {
            let hostURL = URL(fileURLWithPath: share.hostPath)
            let sharedDir = VZSharedDirectory(url: hostURL, readOnly: share.readOnly)
            let fsConfig = VZVirtioFileSystemDeviceConfiguration(tag: share.tag)
            fsConfig.share = VZSingleDirectoryShare(directory: sharedDir)
            dirSharingDevices.append(fsConfig)
            print("VZRunner: Sharing \(share.hostPath) as '\(share.tag)'\(share.readOnly ? " (ro)" : "")", to: &standardError)
        }

        // Rosetta translator share: the guest kernel stays arm64; this exposes
        // the x86_64 translator as a virtiofs device (tag "rosetta") that the
        // guest mounts and registers with binfmt_misc. Install is deliberately
        // not automated here: it is a one-time, user-driven host action.
        if config.rosetta {
            switch VZLinuxRosettaDirectoryShare.availability {
            case .installed:
                break
            case .notInstalled:
                throw NSError(domain: "VZRunner", code: 4, userInfo: [NSLocalizedDescriptionKey:
                    "Rosetta is not installed; run 'softwareupdate --install-rosetta' once and retry"])
            case .notSupported:
                throw NSError(domain: "VZRunner", code: 5, userInfo: [NSLocalizedDescriptionKey:
                    "Rosetta is not supported on this host"])
            @unknown default:
                throw NSError(domain: "VZRunner", code: 6, userInfo: [NSLocalizedDescriptionKey:
                    "unknown Rosetta availability state"])
            }
            let rosettaDev = VZVirtioFileSystemDeviceConfiguration(tag: "rosetta")
            rosettaDev.share = try VZLinuxRosettaDirectoryShare()
            dirSharingDevices.append(rosettaDev)
            print("VZRunner: Rosetta translator shared as 'rosetta'", to: &standardError)
        }
        vmConfig.directorySharingDevices = dirSharingDevices

        // Network (NAT)
        // Single NIC. Default: Apple NAT. With --net-fd, the guest's only
        // interface is instead backed by an inherited datagram socket
        // (VZFileHandleNetworkDeviceAttachment) connected to a user-mode
        // gateway — Apple's NAT isolates guests from each other, and this
        // keeps guest-visible topology identical to urunc on Linux (one
        // kernel-configured interface).
        //
        // With --no-net there is no NIC in the configuration at all, which is
        // the only honest way to serve `hull run --net none` here: an attached
        // device with nothing behind it would still be a device the guest can
        // see, and the framework has no "detached" attachment. Until this
        // existed the flag was accepted by hull and dropped on the floor
        // before it reached us, so the guest came up on Apple NAT with a
        // default route to the internet.
        if config.noNet {
            vmConfig.networkDevices = []
            print("VZRunner: no network device attached (--no-net)", to: &standardError)
        } else {
            let netConfig = VZVirtioNetworkDeviceConfiguration()
            if config.netFD >= 0 {
                netConfig.attachment = VZFileHandleNetworkDeviceAttachment(
                    fileHandle: FileHandle(fileDescriptor: config.netFD)
                )
                print("VZRunner: NIC backed by gateway fd \(config.netFD)", to: &standardError)
            } else {
                netConfig.attachment = VZNATNetworkDeviceAttachment()
            }
            if !config.macAddress.isEmpty {
                guard let mac = VZMACAddress(string: config.macAddress) else {
                    print("VZRunner: invalid --mac '\(config.macAddress)'", to: &standardError)
                    exit(1)
                }
                netConfig.macAddress = mac
            }
            vmConfig.networkDevices = [netConfig]
        }

        // vsock device for the urunit-agent exec transport
        if !config.agentSockPath.isEmpty {
            vmConfig.socketDevices = [VZVirtioSocketDeviceConfiguration()]
        }

        // Validate
        do {
            try vmConfig.validate()
            print("VZRunner: VM configuration validated", to: &standardError)
        } catch {
            print("VZRunner: VM configuration validation failed: \(error)", to: &standardError)
            throw error
        }

        // With a state dir the configuration must also be snapshottable.
        // Fatal when restoring (it cannot work); a warning otherwise, so an
        // unsupported device degrades checkpointing, not the workload.
        if !config.stateDir.isEmpty {
            do {
                try vmConfig.validateSaveRestoreSupport()
            } catch {
                if config.restore {
                    print("VZRunner: configuration cannot be restored: \(error)", to: &standardError)
                    throw error
                }
                print("VZRunner: warning: checkpointing unavailable for this configuration: \(error)", to: &standardError)
            }
        }

        let vm = VZVirtualMachine(configuration: vmConfig)
        vm.delegate = self
        self.vm = vm
        return vm
    }

    func shutdown() {
        // A second signal while a graceful stop is pending forces an
        // immediate stop — mashing Ctrl-C must never leave the runner
        // holding the terminal.
        if isShuttingDown, let vm = self.vm {
            print("VZRunner: second signal, forcing stop", to: &standardError)
            vm.stop { _ in exit(0) }
            return
        }
        if !isShuttingDown, let vm = self.vm {
            isShuttingDown = true
            print("VZRunner: Graceful shutdown initiated", to: &standardError)
            do {
                try vm.requestStop()
                // Guests without ACPI handling (bare init processes) never
                // answer requestStop; force-stop after the configured grace
                // (wired from run --stop-grace, docker's stop_grace_period)
                // so the runner exits promptly instead of being SIGKILLed.
                let grace = DispatchTimeInterval.seconds(self.config.stopGraceSeconds)
                DispatchQueue.main.asyncAfter(deadline: .now() + grace) {
                    // A guest that already stopped (or is mid-stop) must not
                    // be forced; the delegate's exit path wins.
                    if vm.state == .stopped || vm.state == .stopping {
                        return
                    }
                    print("VZRunner: guest ignored stop request, forcing stop", to: &standardError)
                    vm.stop { _ in exit(0) }
                }
            } catch {
                print("VZRunner: Error requesting stop: \(error)", to: &standardError)
                vm.stop { _ in exit(0) }
            }
        }
    }

    // checkpoint pauses the guest, saves the machine state and an APFS clone
    // of the rootfs disk into the state dir, and resumes. Triggered by
    // SIGUSR1. The manifest (latest.json) is written last, so its mtime is
    // the "checkpoint finished" signal for the CLI.
    func checkpoint() {
        guard !config.stateDir.isEmpty else {
            print("VZRunner: checkpoint requested but no --state-dir configured", to: &standardError)
            return
        }
        guard let vm = self.vm, vm.state == .running, !isShuttingDown, !isCheckpointing else {
            print("VZRunner: checkpoint requested but VM is not in a checkpointable state", to: &standardError)
            return
        }
        isCheckpointing = true
        let started = Date()
        print("VZRunner: checkpoint: pausing VM", to: &standardError)
        fflush(stderr)
        vm.pause { pauseResult in
            if case .failure(let error) = pauseResult {
                print("VZRunner: checkpoint: pause failed: \(error)", to: &standardError)
                self.isCheckpointing = false
                return
            }
            try? FileManager.default.removeItem(atPath: self.config.vmStatePath)
            vm.saveMachineStateTo(url: URL(fileURLWithPath: self.config.vmStatePath)) { saveError in
                var diskCloned = false
                if let saveError = saveError {
                    print("VZRunner: checkpoint: state save failed: \(saveError)", to: &standardError)
                } else if !self.config.rootfsPath.isEmpty {
                    // Clone the paused disk next to the machine state so the
                    // pair is consistent; cp -c is an APFS clonefile (CoW).
                    try? FileManager.default.removeItem(atPath: self.config.diskStatePath)
                    let status = (try? runShellQuietly("/bin/cp",
                        ["-c", self.config.rootfsPath, self.config.diskStatePath])) ?? 1
                    diskCloned = status == 0
                    if !diskCloned {
                        print("VZRunner: checkpoint: disk clone failed (cp -c exit \(status))", to: &standardError)
                    }
                }
                if saveError == nil {
                    let elapsedMs = Int(Date().timeIntervalSince(started) * 1000)
                    let manifest: [String: Any] = [
                        "stateFile": "vm.vzstate",
                        "diskImage": diskCloned ? "rootfs.img" : "",
                        "savedAt": ISO8601DateFormatter().string(from: started),
                        "durationMs": elapsedMs,
                    ]
                    if let data = try? JSONSerialization.data(withJSONObject: manifest, options: [.sortedKeys]) {
                        try? data.write(to: URL(fileURLWithPath: self.config.manifestPath))
                    }
                    print("VZRunner: checkpoint saved in \(elapsedMs)ms", to: &standardError)
                }
                fflush(stderr)
                vm.resume { resumeResult in
                    if case .failure(let error) = resumeResult {
                        print("VZRunner: checkpoint: resume failed: \(error)", to: &standardError)
                        fflush(stderr)
                        // A guest stuck in pause is useless; give up cleanly.
                        vm.stop { _ in exit(1) }
                        return
                    }
                    print("VZRunner: checkpoint: VM resumed", to: &standardError)
                    fflush(stderr)
                    self.isCheckpointing = false
                }
            }
        }
    }

    // restoreDisk puts the checkpointed rootfs clone back in place of the
    // live disk image. Must run before createVM attaches the file.
    func restoreDisk() throws {
        guard !config.rootfsPath.isEmpty else { return }
        guard FileManager.default.fileExists(atPath: config.diskStatePath) else {
            print("VZRunner: restore: no disk image in checkpoint, keeping current rootfs", to: &standardError)
            return
        }
        try? FileManager.default.removeItem(atPath: config.rootfsPath)
        let status = try runShellQuietly("/bin/cp", ["-c", config.diskStatePath, config.rootfsPath])
        guard status == 0 else {
            throw NSError(domain: "VZRunner", code: 3,
                userInfo: [NSLocalizedDescriptionKey: "failed to restore rootfs from checkpoint (cp -c exit \(status))"])
        }
        print("VZRunner: restore: rootfs restored from checkpoint", to: &standardError)
    }

    func run() {
        fflush(stderr)

        do {
            if config.restore {
                guard FileManager.default.fileExists(atPath: config.vmStatePath) else {
                    print("VZRunner: restore: no saved state at \(config.vmStatePath)", to: &standardError)
                    exit(1)
                }
                try restoreDisk()
            }
            let vm = try createVM()

            let bootMode = config.useDirectBoot ? "direct" : "EFI"
            print("VZRunner: Starting VM (\(bootMode) boot)", to: &standardError)
            print("  Kernel: \(config.kernelPath)", to: &standardError)
            if !config.initrdPath.isEmpty { print("  Initrd: \(config.initrdPath)", to: &standardError) }
            if !config.rootfsPath.isEmpty { print("  Rootfs: \(config.rootfsPath)", to: &standardError) }
            print("  Cmdline: \(config.cmdline)", to: &standardError)
            print("  Memory: \(config.memoryMB)MB, CPUs: \(config.cpuCount)", to: &standardError)
            fflush(stderr)

            signal(SIGTERM, signalHandler)
            signal(SIGINT, signalHandler)
            signal(SIGUSR1, checkpointSignalHandler)

            if config.restore {
                print("VZRunner: restoring VM state from \(config.vmStatePath)", to: &standardError)
                fflush(stderr)
                vm.restoreMachineStateFrom(url: URL(fileURLWithPath: config.vmStatePath)) { error in
                    if let error = error {
                        print("VZRunner: VM restore failed: \(error)", to: &standardError)
                        fflush(stderr)
                        exit(1)
                    }
                    vm.resume { result in
                        switch result {
                        case .failure(let error):
                            print("VZRunner: VM failed to resume after restore: \(error)", to: &standardError)
                            fflush(stderr)
                            exit(1)
                        case .success:
                            print("VZRunner: VM restored and resumed", to: &standardError)
                            fflush(stderr)
                            if !self.config.agentSockPath.isEmpty {
                                self.startAgentBridge(vm: vm, path: self.config.agentSockPath)
                            }
                        }
                    }
                }
            } else {
                vm.start { result in
                    switch result {
                    case .failure(let error):
                        print("VZRunner: VM failed to start: \(error)", to: &standardError)
                        fflush(stderr)
                        exit(1)
                    case .success:
                        print("VZRunner: VM started successfully", to: &standardError)
                        fflush(stderr)
                        if !self.config.agentSockPath.isEmpty {
                            self.startAgentBridge(vm: vm, path: self.config.agentSockPath)
                        }
                    }
                }
            }
        } catch {
            print("VZRunner: Error: \(error)", to: &standardError)
            exit(1)
        }
    }

    // MARK: - GUI window (--gui)

    /// Bring up AppKit and show the guest display in a native window bound to
    /// the VM. Must be called on the main thread, after run() has created the
    /// VM (self.vm). The caller then drives NSApp.run() to keep it alive.
    /// The VM and its VZVirtualMachineView both live on the main thread.
    func startGUI() {
        guard let vm = self.vm else {
            print("VZRunner: --gui: no VM to display", to: &standardError)
            return
        }

        let app = NSApplication.shared
        // .regular gives the process a Dock icon and lets its window become
        // key/front even though it launched from a CLI without an app bundle.
        app.setActivationPolicy(.regular)

        let frame = NSRect(x: 0, y: 0, width: 1280, height: 800)
        let vmView = VZVirtualMachineView(frame: frame)
        vmView.virtualMachine = vm
        vmView.automaticallyReconfiguresDisplay = true  // macOS 14+

        let window = NSWindow(
            contentRect: frame,
            styleMask: [.titled, .closable, .miniaturizable, .resizable],
            backing: .buffered,
            defer: false
        )
        window.title = config.guiTitle
        window.contentView = vmView
        window.center()
        window.makeKeyAndOrderFront(nil)
        window.makeFirstResponder(vmView)
        self.window = window

        app.activate(ignoringOtherApps: true)
        print("VZRunner: --gui: window '\(config.guiTitle)' shown", to: &standardError)
        fflush(stderr)
    }

    // MARK: - Agent bridge (host unix socket <-> guest vsock port 1024)

    /// One bridged connection: a host client fd and a guest vsock fd, with a
    /// byte pump per direction. The VZVirtioSocketConnection is retained so
    /// its descriptor stays open for the lifetime of the bridge.
    final class AgentBridge {
        let clientFD: Int32
        let vsockFD: Int32
        let connection: VZVirtioSocketConnection
        private let lock = NSLock()
        private var finished = 0

        init(clientFD: Int32, connection: VZVirtioSocketConnection) {
            self.clientFD = clientFD
            self.connection = connection
            self.vsockFD = connection.fileDescriptor
            // The Virtualization framework hands out non-blocking
            // descriptors; the pumps below use blocking I/O.
            let flags = fcntl(vsockFD, F_GETFL, 0)
            _ = fcntl(vsockFD, F_SETFL, flags & ~O_NONBLOCK)
        }

        func start() {
            Thread.detachNewThread { [self] in pump(from: clientFD, to: vsockFD) }
            Thread.detachNewThread { [self] in pump(from: vsockFD, to: clientFD) }
        }

        private func pump(from: Int32, to: Int32) {
            var buf = [UInt8](repeating: 0, count: 32768)
            while true {
                let n = buf.withUnsafeMutableBytes { p in
                    read(from, p.baseAddress, p.count)
                }
                if n < 0 && (errno == EINTR || errno == EAGAIN) { continue }
                if n <= 0 { break }
                let wrote = buf.withUnsafeBytes { p -> Bool in
                    var off = 0
                    while off < n {
                        let w = write(to, p.baseAddress!.advanced(by: off), n - off)
                        if w < 0 && (errno == EINTR || errno == EAGAIN) { continue }
                        if w <= 0 { return false }
                        off += w
                    }
                    return true
                }
                if !wrote { done(); return }
            }
            Darwin.shutdown(to, SHUT_WR)
            done()
        }

        private func done() {
            lock.lock()
            finished += 1
            let all = finished == 2
            lock.unlock()
            if all {
                close(clientFD)
                connection.close()
            }
        }
    }

    private var bridges: [AgentBridge] = []
    private let bridgesLock = NSLock()

    /// Listen on a unix socket and bridge every client to guest vsock port
    /// 1024, where urunit-agent serves exec sessions.
    func startAgentBridge(vm: VZVirtualMachine, path: String) {
        unlink(path)
        let listenFD = socket(AF_UNIX, SOCK_STREAM, 0)
        guard listenFD >= 0 else {
            print("VZRunner: agent bridge: socket() failed", to: &standardError)
            return
        }
        var addr = sockaddr_un()
        addr.sun_family = sa_family_t(AF_UNIX)
        let pathCapacity = MemoryLayout.size(ofValue: addr.sun_path)
        let ok = path.withCString { cs -> Bool in
            if strlen(cs) >= pathCapacity { return false }
            return withUnsafeMutablePointer(to: &addr.sun_path) { ptr in
                ptr.withMemoryRebound(to: CChar.self, capacity: pathCapacity) { dst in
                    strcpy(dst, cs)
                    return true
                }
            }
        }
        guard ok else {
            print("VZRunner: agent bridge: socket path too long", to: &standardError)
            close(listenFD)
            return
        }
        let bindRes = withUnsafePointer(to: &addr) {
            $0.withMemoryRebound(to: sockaddr.self, capacity: 1) {
                Darwin.bind(listenFD, $0, socklen_t(MemoryLayout<sockaddr_un>.size))
            }
        }
        guard bindRes == 0, listen(listenFD, 8) == 0 else {
            print("VZRunner: agent bridge: bind/listen on \(path) failed: \(String(cString: strerror(errno)))", to: &standardError)
            close(listenFD)
            return
        }
        print("VZRunner: agent bridge listening on \(path)", to: &standardError)
        fflush(stderr)

        Thread.detachNewThread { [weak self] in
            while true {
                let clientFD = accept(listenFD, nil, nil)
                if clientFD < 0 {
                    if errno == EINTR { continue }
                    break
                }
                DispatchQueue.main.async {
                    guard let self, let socketDevice = vm.socketDevices.first as? VZVirtioSocketDevice else {
                        print("VZRunner: agent bridge: no vsock device on running VM (devices: \(vm.socketDevices.count))", to: &standardError)
                        fflush(stderr)
                        close(clientFD)
                        return
                    }
                    print("VZRunner: agent bridge: client connected, dialing guest port 1024", to: &standardError)
                    fflush(stderr)
                    socketDevice.connect(toPort: 1024) { result in
                        switch result {
                        case .failure(let error):
                            print("VZRunner: agent bridge: vsock connect failed: \(error)", to: &standardError)
                            fflush(stderr)
                            close(clientFD)
                        case .success(let connection):
                            print("VZRunner: agent bridge: guest connection established (fd \(connection.fileDescriptor))", to: &standardError)
                            fflush(stderr)
                            let bridge = AgentBridge(clientFD: clientFD, connection: connection)
                            self.bridgesLock.lock()
                            self.bridges.append(bridge)
                            self.bridgesLock.unlock()
                            bridge.start()
                        }
                    }
                }
            }
        }
    }

    // MARK: - VZVirtualMachineDelegate

    nonisolated func virtualMachineDidStop(_ virtualMachine: VZVirtualMachine) {
        drainConsole()
        print("VZRunner: VM stopped", to: &standardError)
        exit(0)
    }

    nonisolated func virtualMachine(_ virtualMachine: VZVirtualMachine, didStopWithError error: Error) {
        drainConsole()
        print("VZRunner: VM stopped with error: \(error)", to: &standardError)
        exit(1)
    }
}

// MARK: - Utilities

var standardOutput = FileHandle.standardOutput
var standardError = FileHandle.standardError

extension FileHandle: @retroactive TextOutputStream {
    public func write(_ string: String) {
        let data = Data(string.utf8)
        self.write(data)
    }
}

// MARK: - Main

@main
struct VzRunner {
    static func main() {
        let args = CommandLine.arguments

        // Internal: filter stdin to stdout and exit, so the console policy can
        // be exercised from a shell without booting a VM. The runner is an
        // executable target with top-level code, which SwiftPM cannot host in
        // a test target; this is the seam that keeps the policy testable.
        if args.count > 1 && args[1] == "--filter-selftest" {
            var sanitizer = TerminalSanitizer()
            while let chunk = try? FileHandle.standardInput.read(upToCount: 65536), !chunk.isEmpty {
                writeAllToStdout(sanitizer.filter([UInt8](chunk)))
            }
            exit(0)
        }

        guard let config = VMConfig.parse(args) else {
            print("Usage: vz-runner --kernel <path> [--initrd <path>] [--rootfs <path>] [--cmdline <string>] [--mem MB] [--cpus N] [--efi] [--share <path> <tag>] [--mac <address>] [--net-fd <n>] [--stop-grace <s>] [--qmp socket] [--state-dir <dir>] [--restore] [--gui] [--gui-title <str>] [--rosetta]", to: &standardError)
            exit(1)
        }

        let manager = VZManager(config: config)
        gManager = manager

        // VZVirtualMachine requires the main dispatch queue.
        manager.run()

        if config.gui {
            // GUI mode: host the guest display in an AppKit window on the main
            // thread and pump the main run loop via NSApp. The VM was already
            // started on the main queue by run(); its completion handlers and
            // the VM delegate (which calls exit()) are serviced here too.
            manager.startGUI()
            NSApplication.shared.run()
        } else {
            // Headless: RunLoop keeps the main thread alive; the VM delegate
            // calls exit() when the guest stops.
            RunLoop.main.run()
        }
    }
}
