#!/usr/bin/env bash
# Checks the parts of the committed iOS project that are promises to the user
# rather than implementation details.
#
# The project is committed and edited by hand (and by Xcode), so any of these
# can change in a diff nobody reads closely: a library's install instructions
# that add a permission string, a template upgrade that loosens App Transport
# Security, a merge that loses the privacy manifest's DiskSpace declaration and
# gets the next upload flagged.
set -euo pipefail

ios="$(cd "$(dirname "$0")/../ios" && pwd)"

python3 - "$ios" <<'PY'
import plistlib, sys, pathlib, re
ios = pathlib.Path(sys.argv[1])
info = plistlib.loads((ios / "Boxwright" / "Info.plist").read_bytes())
privacy = plistlib.loads((ios / "Boxwright" / "PrivacyInfo.xcprivacy").read_bytes())
pbx = (ios / "Boxwright.xcodeproj" / "project.pbxproj").read_text()
failures = []

def check(ok, message):
    print(("ok   " if ok else "FAIL ") + message)
    if not ok:
        failures.append(message)

ats = info.get("NSAppTransportSecurity", {})
check(ats.get("NSAllowsArbitraryLoads") is False, "ATS: arbitrary http loads are refused")
check(ats.get("NSAllowsLocalNetworking") is True, "ATS: http to local-network addresses is allowed")
check(not ats.get("NSExceptionDomains"), "ATS: no per-domain exceptions")
check(info.get("ITSAppUsesNonExemptEncryption") is False, "export compliance: no non-exempt encryption")
check(bool(info.get("NSLocalNetworkUsageDescription")), "local network permission is explained")
check(bool(info.get("NSCameraUsageDescription")), "camera permission is explained")
# Every colour in the app assumes a white background; see the StatusBar in App.tsx.
check(info.get("UIUserInterfaceStyle") == "Light", "appearance is pinned to light until a dark palette exists")

# Every permission string is a question iOS may put to the user. Only ask for
# what the app uses; a new one here must be a deliberate edit to this list.
#
# The photo-library string is one iOS never shows. "Choose an existing photo"
# is the system picker, which needs no permission -- but react-native-image-
# picker LINKS PHPhotoLibrary, and App Store Connect rejects any upload whose
# code references it without the string (ITMS-90683; the first TestFlight
# upload, 2026-09-29). It stays unshown only while the picker is asked for no
# asset metadata: includeExtra true would make it request library access, so
# that is checked below rather than trusted.
allowed = {"NSCameraUsageDescription", "NSLocalNetworkUsageDescription", "NSPhotoLibraryUsageDescription"}
asked = {k for k in info if k.endswith("UsageDescription")}
check(asked <= allowed, f"no unexpected permission prompts (extra: {sorted(asked - allowed) or 'none'})")
check(all(info[k] for k in asked), "no permission string is empty")
app_source = (ios.parent / "App.tsx").read_text()
check(
    "includeExtra: false" in app_source and "includeExtra: true" not in app_source,
    "the photo picker never asks for library access (includeExtra: false), so the photo-library string is never shown",
)

# LiDAR capture (modules/boxwright-depth) runs on ARKit, and ARKit reads the
# motion sensors itself: it needs no motion permission, and the camera string
# above is the only prompt it causes. Measuring is an extra on Pro phones, never
# a requirement to install -- an "arkit" (or depth) entry here would hide the
# app from every iPhone without one.
check("NSMotionUsageDescription" not in info, "no motion permission: ARKit needs none")
caps = info.get("UIRequiredDeviceCapabilities", [])
cap_names = [str(c).lower() for c in (caps.keys() if isinstance(caps, dict) else caps)]
depth_caps = [c for c in cap_names if "arkit" in c or "lidar" in c or "depth" in c]
check(not depth_caps, f"installs without LiDAR: no ARKit/depth device requirement (found {depth_caps or 'none'})")

# Principle 5: no telemetry, ever. The manifest is where that is declared to Apple.
check(privacy.get("NSPrivacyTracking") is False, "privacy manifest: no tracking")
check(privacy.get("NSPrivacyCollectedDataTypes") == [], "privacy manifest: collects no data")
declared = {t["NSPrivacyAccessedAPIType"] for t in privacy.get("NSPrivacyAccessedAPITypes", [])}
# react-native-file-access ships no manifest of its own but its binary reads
# free space and modification dates; Apple's upload scan flags both.
for api in ["NSPrivacyAccessedAPICategoryDiskSpace", "NSPrivacyAccessedAPICategoryFileTimestamp",
            "NSPrivacyAccessedAPICategoryUserDefaults", "NSPrivacyAccessedAPICategorySystemBootTime"]:
    check(api in declared, f"privacy manifest declares {api.removeprefix('NSPrivacyAccessedAPICategory')}")

# Bundled fonts are resources, listed by file name. Each one must be in the
# project's resources (or iOS silently falls back to the system font) and ship
# with its licence beside it.
fonts = info.get("UIAppFonts", [])
fonts_dir = ios.parent / "assets" / "fonts"
check(all(f in pbx and (fonts_dir / f).exists() for f in fonts), f"bundled fonts are in the project and on disk ({fonts or 'none'})")
check(not fonts or (fonts_dir / "OFL.txt").exists(), "bundled fonts ship with their licence")

ids = set(re.findall(r"PRODUCT_BUNDLE_IDENTIFIER = ([^;]+);", pbx))
check(ids == {"app.boxwright"}, f"bundle identifier is app.boxwright everywhere (found {sorted(ids)})")
families = set(re.findall(r"TARGETED_DEVICE_FAMILY = ([^;]+);", pbx))
check(families == {"1"}, "iPhone only")
check((ios / "Boxwright" / "Images.xcassets" / "AppIcon.appiconset" / "AppIcon-1024.png").exists(), "app icon present")

sys.exit(1 if failures else 0)
PY
