# XIAO Remote for Windows

A XIAO ESP32S3 contact closure requests YouTube playback on a Windows PC over the internet. The Windows receiver is a native GUI-subsystem executable: it runs in the background without a console window, Python, or a PowerShell listener.

## Windows use

Windows 10/11 x64, internet access, and Chrome or Edge are required. The PC must be awake and logged in. The XIAO needs its configured 2.4 GHz Wi-Fi network. Both devices may use different networks. No router port forwarding is needed.

The private Flipper scripts contain pairing information and are generated locally. They are **not published in this repository or releases**. First installation downloads the versioned executable and verifies its pinned SHA256 before execution. Later starts use the installed executable directly.

Installation: `%LOCALAPPDATA%\XiaoRemote\Receiver\xiao.exe`.

Commands:

```text
xiao.exe run          Start the receiver in the background
xiao.exe stop         Stop the receiver, keeping settings
xiao.exe status       Open the current status in Notepad
xiao.exe test         Open the configured YouTube video for an audio/playback test
xiao.exe uninstall    Stop and remove this application, pairing, state and dedicated browser profiles
```

Closing a terminal is not needed. Start the receiver again after Windows login/reboot; automatic startup is not registered. Its process remains visible in Task Manager. Error messages use normal Windows dialogs. Antivirus, SmartScreen, and managed-device policies remain in effect.

The uninstaller only removes the application's own `%LOCALAPPDATA%\XiaoRemote` folder and its named installer temporary files. It stops the former receiver only when the exact `-File` argument names the installed `receiver.ps1`. It closes only browser processes with this application's dedicated `--user-data-dir`. Personal browser profiles, unrelated PowerShell processes, and Windows history/event logs are not removed.

## Pairing and relay

The configuration file uses the schema in `windows/config.example.json`. A locally generated 192-bit topic and a separate 256-bit HMAC key must match the XIAO firmware. Keep actual config files, firmware binaries and private Flipper payloads private. The release executable contains no pairing information or Wi-Fi credentials.

Events use certificate-verified HTTPS through [ntfy.sh](https://docs.ntfy.sh/subscribe/api/), HMAC-SHA256, a 30-second validity window, a receiver start-time check, and persisted event IDs. Retry duplicates do not open extra windows. Rapid extra signals within two seconds are discarded. A relay HTTP 200 is not confirmation of Windows delivery or video/audio playback. ntfy.sh is an external best-effort service with usage limits.

## Build and tests

No third-party Go dependencies.

```sh
cd native
go test -race ./...
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -buildvcs=false -ldflags '-H=windowsgui -s -w' -o xiao.exe .
```

Windows-specific tests build and launch the real GUI executable in an isolated temporary user-data folder, confirm that it has no console, exercise duplicate start and stop, uninstall it, and check that unrelated files remain intact. Test data never uses production pairing keys. Physical Flipper keystroke timing and actual YouTube/audio behavior still require validation on the target PC.
