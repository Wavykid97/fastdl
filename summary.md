## Laneway 2.1.1 on macOS 15.8.2 on arm64

| | Check | Details |
|---|---|---|
| ✅ | Release is described in latest-mac.json | version 2.1.1 |
| ✅ | The .dmg opens |  |
| ✅ | The .dmg contains Laneway.app |  |
| ✅ | The .dmg has the Applications shortcut |  |
| ✅ | Laneway copies into Applications |  |
| ✅ | Version in the app matches the release | 2.1.1 |
| ✅ | Runs on both Intel and Apple-silicon Macs | arm64 x86_64 |
| ✅ | Signature is intact (nothing changed since it was built) | /Applications/Laneway.app: satisfies its Designated Requirement |
| ✅ | Gatekeeper reaction (expected: rejected until Apple-notarized) | /Applications/Laneway.app: rejected |
| ✅ | Laneway opens |  |
| ✅ | Laneway is running and answering | version 2.1.1 |
| ✅ | Running version is the release | 2.1.1 |
| ✅ | Laneway connected itself to Chrome (browser helper file written) | /Users/runner/Library/Application Support/Google/Chrome/NativeMessagingHosts/com.laneway.app.json |
| ✅ | …pointing at the app in Applications | /Applications/Laneway.app/Contents/MacOS/Laneway |
| ✅ | …and trusting the Chrome Web Store extension |  |
| ✅ | The browser extension can reach Laneway | {'ok': True, 'version': '2.1.1', 'running': True} |
| ✅ | Downloads a real file | 6227005 bytes in 0.2 s with 4 connections |
| ✅ | …and the file is exactly right (SHA-256 matches) | d1360f9dcf644f2e… |
| ✅ | Laneway quits cleanly |  |

Screenshots are attached to this run (Artifacts, at the bottom of the run's page).

### Browser extension

| | Check | Details |
|---|---|---|
| ✅ | Laneway's helper file lets the folder copy of the extension in |  |
| ✅ | The extension loads |  |
| ✅ | The extension finds Laneway | {"ok":true,"version":"2.1.1","running":false} |
| ✅ | Clicking a file link sends it to Laneway | downloading |
| ✅ | …and Laneway downloads it | done |
| ✅ | The extension's popup is connected | Connected to Laneway 2.1.1 |
