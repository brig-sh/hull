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
                if i < args.count { config.memoryMB = Int(args[i]) ?? 512 }
            case "--cpus":
                i += 1
                if i < args.count { config.cpuCount = Int(args[i]) ?? 1 }
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
                if i < args.count { config.netFD = Int32(args[i]) ?? -1 }
            case "--stop-grace":
                i += 1
                if i < args.count { config.stopGraceSeconds = Int(args[i]) ?? 10 }
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
            case "--share":
                // Format: --share <path> <tag>
                // Path and tag are separate arguments to avoid colon-splitting issues
                i += 1
                guard i < args.count else { break }
                let hostPath = args[i]
                i += 1
                guard i < args.count else { break }
                let tag = args[i]
                config.sharedDirs.append(SharedDir(hostPath: hostPath, tag: tag, readOnly: false))
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

    init(config: VMConfig) {
        self.config = config
        super.init()
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
        let requestedMem = UInt64(config.memoryMB) * 1024 * 1024
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

        // Serial console — attach directly to stdin/stdout
        let serialConfig = VZVirtioConsoleDeviceSerialPortConfiguration()
        let serialAttachment = VZFileHandleSerialPortAttachment(
            fileHandleForReading: FileHandle.standardInput,
            fileHandleForWriting: FileHandle.standardOutput
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
        print("VZRunner: VM stopped", to: &standardError)
        exit(0)
    }

    nonisolated func virtualMachine(_ virtualMachine: VZVirtualMachine, didStopWithError error: Error) {
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
