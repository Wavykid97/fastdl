Laneway - multi-connection downloader
=====================================
(formerly called FastDL)

INSTALLING
  Run the latest Laneway-Setup-x.y.z.exe. It installs just for you (no admin
  prompt), adds Laneway to the Start menu, and can add a desktop shortcut. Running it
  over an older version upgrades it and keeps your downloads and settings.
  Uninstall it from Settings > Apps like any other program.

  Windows may show "Windows protected your PC" the first time, because the
  installer isn't code-signed. Click "More info" -> "Run anyway".

USING IT
  Open Laneway from the Start menu. Paste a link (or drag one onto the
  window), pick how many connections to use, and click Download.
  Each download's bar is split into the pieces being fetched in parallel,
  so you can watch the connections work.

  DOWNLOAD DETAILS
  Click a download (or its arrow) to open its details: speed now, average
  and fastest speed, time spent, the server, whether it can resume, and a
  Connections table showing what each connection is doing: which part of
  the file it's fetching, how much it has downloaded, its speed and retries.
  Tabs (All, Active, Paused, Finished, Failed) and search sort the list.

  PICKING UP WHERE IT STOPPED
  - If the connection drops, Laneway retries by itself (10 s, 20 s, 40 s...
    up to every 5 minutes) and carries on from where it stopped.
  - Downloads interrupted by closing Laneway, an update, a crash or a power
    cut carry on the next time Laneway opens. Ones you paused stay paused.
  - Progress is flushed to disk every second, so it's accurate even after a
    power cut. If the file on the server changed in the meantime (checked
    with its ETag/Last-Modified), Laneway starts it over instead of
    stitching two different files together.
  Both are on by default and can be switched off in Settings.
  (Servers that don't support partial downloads can't resume; Laneway
  shows this in the details.)

  BROWSER EXTENSION (Chrome, Edge, Brave)
  With the extension on, downloads you start in the browser go to Laneway,
  including ones that need you to be signed in (it passes along the site's
  cookies for that file). Plain clicks on links to files (videos, music,
  zips, installers) go to Laneway too, even ones the browser would open in
  a tab; Ctrl+click opens a link in the browser instead. Right-click a link
  for "Download with Laneway" for anything else.
  The "Get the browser extension" button in Laneway's header links to
  extensionStoreURL in gui.go. It's a PLACEHOLDER until the extension is on
  the Chrome Web Store; until then the button shows the setup steps. Put the
  real store address there and release an update.
  The extension's popup (click its toolbar icon) has:
  - a power button: off means the browser downloads everything itself
  - Skip this site: leave the current site's downloads to the browser
    (right-click "Download with Laneway" still works there)
  - Downloads: live progress with pause/resume/open/show-in-folder
  - This page: every file linked on the current page, filterable by type,
    to tick and send to Laneway in one go
  - Settings: catch links to files, notifications, show Laneway, and the
    list of skipped sites
  When a download sent from the browser finishes or fails, the browser
  shows a notification (with Open / Show in folder); Laneway doesn't show
  a second one for those. If Laneway isn't installed, the browser just
  downloads as normal.
  Setting it up (Settings > Browser extension > Set up shows these steps):
    1. Go to chrome://extensions (edge://extensions in Edge)
    2. Turn on Developer mode
    3. Load unpacked > choose the "extension" folder in Laneway's install
       folder (%LOCALAPPDATA%\Programs\Laneway\extension)
  How it connects: the installer registers Laneway.exe as a "native
  messaging host" (native-host.json) for Chrome, Edge, Brave and Chromium.
  Only the Laneway extension's ID may use it, so websites can't.
  Publishing to the Chrome Web Store ($5 one-time developer fee) or Edge
  Add-ons (free) makes it a one-click install: upload Laneway-extension-store.zip
  (same files, without the "key" line). The store gives the extension a new
  ID; add "chrome-extension://THAT-ID/" to allowed_origins in
  build/native-host.json and release a Laneway update.

  YOUR OWN WINDOW
  Laneway opens in its own window (using Windows' built-in WebView2), so it
  has its own icon and name on the taskbar and can be pinned. When a
  download finishes while it's in the background, its taskbar button
  flashes. If WebView2 is missing, it opens in an Edge window instead.

  THE RENAME
  Installing Laneway over FastDL (by hand or via auto-update) removes FastDL
  and its shortcuts; Laneway moves the download list, settings and any
  unfinished downloads over the first time it runs. The GitHub repository
  can stay named "fastdl": it's only the update address, users never see it.

  WHAT'S NEW
  After each update Laneway shows what changed (Settings > What's new shows
  the full history). The notes come from changelog.json, which the release
  tool fills in from -notes. Write one item per line; "Title: details"
  shows the title in bold.

  FEATURES (1.1)
  - Smarter speed: a connection that finishes early takes over half of
    the slowest one's remaining work, and frozen connections restart
    after 15 seconds. Downloads with uneven connections finish ~2x faster.
  - Queue: choose how many files download at once (Settings). The rest
    wait in line and start automatically. "Start now" skips the line.
  - Speed limit: cap Laneway's total speed so browsing and calls stay
    smooth (Settings).
  - Paste or drop several links at once.
  - Open finished files directly; Pause all / Resume all.
  - Change link: if a download link expires, paste a fresh one and keep
    your progress (Laneway checks it's the same file).
  - Notifications when a download finishes or fails.
  - Your PC won't fall asleep while something is downloading.
  - Clearer error messages (expired links, missing files, busy servers).

  Closing the window quits Laneway. Unfinished downloads are paused and
  will be waiting, ready to resume, the next time you open it.
  Opening Laneway while it's already running just brings up its window.

UPDATES
  Laneway checks for a new version when it opens and every 6 hours.
  - Required update: Laneway locks with an "Update required" screen until
    it's updated. Running downloads are paused and saved, and carry on
    after the update. One click downloads, checks and installs it, and
    Laneway reopens by itself on the new version.
  - Optional update: a banner offers it; "Later" hides it for that version.
  - If the update server can't be reached, Laneway keeps working, unless it
    has already seen a required update, in which case it stays locked.
  - Every update is signed. Laneway refuses any update that isn't signed
    with your private key or whose installer doesn't match its checksum.
  Settings shows the current version and a "Check for updates" link.

RELEASING A NEW VERSION
  One-time setup:
  1. Create a public GitHub repository named "fastdl" under Wavykid97
     (release.json already points there).
  2. Put the signing key file at  %USERPROFILE%\.laneway\signing.key
     (the old .fastdl folder works too)
     (or pass -key PATH). Never put it in the repository.
  Each release:
     go run ./tools/release build -version 1.4.0 -notes "Title: what changed
     Another item on its own line"
  This builds Laneway.exe and the installer, signs them, and puts
  Laneway-Setup-1.7.0.exe and latest.json in release\v1.7.0\.
  Then on GitHub: Releases > Draft a new release > tag v1.7.0 > attach
  both files > Publish. (Or add -publish if the GitHub CLI "gh" is set up.)
  Updates are required by default. Add -optional for ones users may skip,
  or -min 1.2.0 to only force versions older than 1.2.0.
  Always increase the version number; Laneway only updates to newer ones.

  The signing key is the one thing you must not lose or leak:
  - Lose it and you can never ship another update to existing installs.
  - Leak it and someone else could push "updates" to your users.
  Keep a backup somewhere safe (a password manager or a USB stick).

COMMAND LINE
  Laneway.exe [-n 8] [-o filename] [-retries 5] [-limit MB/s] URL
  Press Ctrl+C to pause; run the same command again to resume.

HOW IT WORKS
  1. Asks the server for byte 0 only (Range: bytes=0-0) to learn the file
     size and whether partial downloads are allowed.
  2. Splits the file into N byte ranges and opens N connections at once.
  3. Each connection writes directly into its spot in a pre-allocated file.
     When a connection finishes, it splits the biggest unfinished range in
     half and takes the back half (never smaller than 1 MB).
  4. Progress is saved every second to "<file>.laneway", which is what makes
     pause/resume work. It is deleted when the download finishes.
  Servers that don't support ranges fall back to a single connection.

  The window is a small web page served only to this PC (127.0.0.1) and
  shown in Microsoft Edge's app mode, with a secret key so other websites
  can't control Laneway. Its list and settings live in %AppData%\Laneway.

SOURCE FILES
  main.go            command-line mode and progress bar
  download.go        the download engine (ranges, splitting, retries, resume)
  limit.go           the shared speed limiter
  updater.go         update checks, lock screen logic, installing updates
  updatekey.go       public key that verifies updates (safe to share)
  internal/update/   the signed release format (shared with the tool)
  tools/release/     builds, signs and publishes releases
  release.json       where updates are published
  changelog.json     release notes shown on the "What's new" screen
  whatsnew.go        decides when to show "What's new"
  gui.go             the app: window, queue, settings, single-instance
  ui.html            the app's interface
  win_windows.go     Windows bits (error dialogs, console, keep-awake)
  window_windows.go  Laneway's own window (taskbar icon, flashing)
  names.go           the app name, and moving over from FastDL
  nativehost.go      talks to the browser extension (native messaging):
                     requests, live download updates, buttons
  extension/         the Chrome/Edge extension
  win_other.go       stand-ins for other systems
  vendor/            copies of the libraries used, so it builds offline
  setup/             the installer and uninstaller (its own small program,
                     with its window in setup/ui.html)
  build/             icon, version info and browser helper registration

BUILDING (needs Go from https://go.dev/dl/)
  For releases use the release tool above. A plain test build:
    go build -ldflags "-s -w -H windowsgui" -o build\Laneway.exe .
  reports its version as "dev" and never checks for updates.
  (rsrc_windows_amd64.syso holds the icon and version info; rebuild it from
   build\laneway.rc with windres if you change the icon.)
  The installer packs build\Laneway.exe, build\native-host.json and the
  extension folder into setup\payload.zip; the release tool does all of it.
  Installer switches: /S installs silently (auto-updates use /S /relaunch),
  /D=C:\folder picks the folder. Uninstall.exe /S removes it silently.
