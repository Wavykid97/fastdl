#!/usr/bin/env python3
"""Tests a Laneway Mac release on a real Mac (run by .github/workflows/mac-test.yml).

It does what a person would: opens the .dmg, copies Laneway into
Applications, opens it, checks it connected itself to the browser, downloads
a real file, and talks to it the way the browser extension does. Results go
to the GitHub job summary, with screenshots attached to the run.
"""
import hashlib, json, os, platform, plistlib, struct, subprocess, sys, time, urllib.request

DMG, MANIFEST, OUT = sys.argv[1], sys.argv[2], sys.argv[3]
os.makedirs(OUT, exist_ok=True)
HOME = os.path.expanduser("~")
SUPPORT = os.path.join(HOME, "Library", "Application Support")
APP = "/Applications/Laneway.app"
EXE = APP + "/Contents/MacOS/Laneway"
STORE_ID = "fkolcpnndjmmnlgagmobpknjhcnfnkdi"
results = []


def check(name, ok, detail=""):
    results.append((name, bool(ok), str(detail)))
    print(("PASS " if ok else "FAIL ") + name + (": " + str(detail) if detail else ""), flush=True)
    return ok


def sh(*args, **kw):
    return subprocess.run(args, capture_output=True, text=True, **kw)


def shot(name):
    r = sh("screencapture", "-x", os.path.join(OUT, name + ".png"))
    print(f"screenshot {name}: {'ok' if r.returncode == 0 else r.stderr.strip()}")


def wait(fn, secs):
    end = time.time() + secs
    while time.time() < end:
        try:
            v = fn()
            if v:
                return v
        except Exception:
            pass
        time.sleep(0.5)
    return None


rel = json.load(open(MANIFEST))
check("Release is described in latest-mac.json", rel.get("version") and rel.get("signature"), "version " + rel.get("version", "?"))

# 1. Open the .dmg like a person would, and look inside.
r = sh("hdiutil", "attach", "-nobrowse", "-readonly", "-mountpoint", "/Volumes/LanewayTest", DMG)
check("The .dmg opens", r.returncode == 0, r.stderr.strip()[-200:])
vol = "/Volumes/LanewayTest"
check("The .dmg contains Laneway.app", os.path.isdir(vol + "/Laneway.app"))
check("The .dmg has the Applications shortcut", os.path.islink(vol + "/Applications") and os.readlink(vol + "/Applications") == "/Applications")
sh("rm", "-rf", APP)
r = sh("cp", "-R", vol + "/Laneway.app", "/Applications/")
check("Laneway copies into Applications", r.returncode == 0, r.stderr.strip())
sh("hdiutil", "detach", vol)

# The window people see when they open the .dmg.
sh("open", DMG)
time.sleep(4)
shot("1-dmg-window")
for v in os.listdir("/Volumes"):
    if v.startswith("Laneway"):
        sh("hdiutil", "detach", "-force", "/Volumes/" + v)

# 2. The app itself.
info = plistlib.load(open(APP + "/Contents/Info.plist", "rb"))
check("Version in the app matches the release", info.get("CFBundleShortVersionString") == rel["version"], info.get("CFBundleShortVersionString"))
archs = sh("lipo", "-archs", EXE).stdout.split()
check("Runs on both Intel and Apple-silicon Macs", "x86_64" in archs and "arm64" in archs, " ".join(archs))
r = sh("codesign", "--verify", "--deep", "--strict", "--verbose=2", APP)
check("Signature is intact (nothing changed since it was built)", r.returncode == 0, (r.stderr or r.stdout).strip().splitlines()[-1] if (r.stderr or r.stdout).strip() else "")
r = sh("spctl", "--assess", "--type", "execute", "-vv", APP)
check("Gatekeeper reaction (expected: rejected until Apple-notarized)", True, (r.stderr or r.stdout).strip().replace("\n", " "))

# 3. Open it. Chrome's folder must exist (it does once Chrome has been used).
os.makedirs(os.path.join(SUPPORT, "Google", "Chrome"), exist_ok=True)
# This test Mac has never opened Chrome; skip Chrome's first-run welcome box
# (people who use Chrome never see it again) so the screenshot shows Laneway.
open(os.path.join(SUPPORT, "Google", "Chrome", "First Run"), "w").close()
inst_file = os.path.join(SUPPORT, "Laneway", "instance.json")
if os.path.exists(inst_file):
    os.remove(inst_file)
r = sh("open", APP)
check("Laneway opens", r.returncode == 0, r.stderr.strip())


def instance():
    i = json.load(open(inst_file))
    req = urllib.request.Request(f"http://{i['addr']}/api/state", headers={"X-Token": i["token"]})
    return i, json.load(urllib.request.urlopen(req, timeout=3))


got = wait(instance, 40)
check("Laneway is running and answering", got, got and ("version " + got[1].get("version", "?")))
if not got:
    print(sh("log", "show", "--last", "2m", "--predicate", 'process == "Laneway"').stdout[-3000:])
inst, state = got if got else ({}, {})
check("Running version is the release", state.get("version") == rel["version"], state.get("version"))
time.sleep(6)
shot("2-laneway-window")

# 4. It told Chrome where it is, so the extension can reach it.
host = os.path.join(SUPPORT, "Google", "Chrome", "NativeMessagingHosts", "com.laneway.app.json")
ok = os.path.exists(host)
m = json.load(open(host)) if ok else {}
check("Laneway connected itself to Chrome (browser helper file written)", ok, host)
check("…pointing at the app in Applications", m.get("path") == os.path.realpath(EXE), m.get("path"))
check("…and trusting the Chrome Web Store extension", f"chrome-extension://{STORE_ID}/" in m.get("allowed_origins", []))


def api(path, body=None):
    req = urllib.request.Request(f"http://{inst['addr']}/api/{path}", data=json.dumps(body).encode() if body is not None else None,
                                 headers={"X-Token": inst["token"], "Content-Type": "application/json"})
    return json.load(urllib.request.urlopen(req, timeout=20))


# 5. Talk to it exactly like the browser extension does (native messaging).
def native(msg):
    p = subprocess.Popen([EXE, f"chrome-extension://{STORE_ID}/"], stdin=subprocess.PIPE, stdout=subprocess.PIPE)
    data = json.dumps(msg).encode()
    p.stdin.write(struct.pack("<I", len(data)) + data)
    p.stdin.flush()
    n = struct.unpack("<I", p.stdout.read(4))[0]
    reply = json.loads(p.stdout.read(n))
    p.stdin.close()
    p.wait(timeout=10)
    return reply


try:
    rep = native({"type": "ping"})
    check("The browser extension can reach Laneway", rep.get("ok") and rep.get("running"), rep)
except Exception as e:
    check("The browser extension can reach Laneway", False, e)

# 6. A real download from GitHub, over several connections, checked byte for byte.
if inst:
    # A public file from the newest published release (the one being tested
    # may still be a draft, whose files aren't public yet).
    url = "https://github.com/Wavykid97/fastdl/releases/latest/download/Laneway.dmg"
    expected = hashlib.sha256(urllib.request.urlopen(url, timeout=120).read()).hexdigest()
    try:
        api("add", {"url": url, "conns": 4})
        def finished():
            for j in api("state")["jobs"]:
                if j.get("url") == url and j["status"] in ("done", "error"):
                    return j
        j = wait(finished, 180)
        ok = j and j["status"] == "done"
        check("Downloads a real file", ok, j and (j.get("error") or f"{j['size']} bytes in {j.get('time', 0):.1f} s with {j.get('conns')} connections"))
        if ok:
            h = hashlib.sha256(open(j["out"], "rb").read()).hexdigest()
            check("…and the file is exactly right (SHA-256 matches)", h == expected, h[:16] + "…")
        shot("3-after-download")
    except Exception as e:
        check("Downloads a real file", False, e)

# 7. Closing it.
if inst:
    try:
        api("quit", {})
    except Exception:
        pass
    gone = wait(lambda: sh("pgrep", "-f", EXE).returncode != 0, 15)
    check("Laneway quits cleanly", gone)

# Summary
mac = f"macOS {platform.mac_ver()[0]} on {platform.machine()}"
lines = [f"## Laneway {rel['version']} on {mac}", "", "| | Check | Details |", "|---|---|---|"]
for name, ok, detail in results:
    lines.append(f"| {'✅' if ok else '❌'} | {name} | {detail.replace('|', '/')[:160]} |")
lines += ["", "Screenshots are attached to this run (Artifacts, at the bottom of the run's page)."]
for target in {os.environ.get("GITHUB_STEP_SUMMARY", ""), os.path.join(OUT, "summary.md")} - {""}:
    with open(target, "a") as f:
        f.write("\n".join(lines) + "\n")
sys.exit(0 if all(ok for _, ok, _ in results) else 1)
