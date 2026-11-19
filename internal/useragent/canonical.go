//
// canonical.go
// Turning browser, system and device names from imported history into the names Parse stores.
//
// Created: 2026-11-19
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package useragent

import "strings"

// importedBrowsers maps the browser names other analytics tools write onto the
// name Parse stores for the same browser. Keys are lower case.
var importedBrowsers = map[string]string{
	"edge":                       BrowserEdge,
	"samsung browser":            "Samsung Internet",
	"yabrowser":                  "Yandex Browser",
	"opera mini":                 "Opera",
	"opera gx":                   "Opera",
	"duckduckgo privacy browser": "DuckDuckGo",
	"android webview":            BrowserMobileApp,
	"safari (in-app)":            BrowserMobileApp,
}

// importedSystems maps the operating system names other analytics tools write
// onto the name Parse stores. Keys are lower case.
var importedSystems = map[string]string{
	"mac":       "macOS",
	"macintosh": "macOS",
	"mac os x":  "macOS",
	"os x":      "macOS",
	"linux":     "GNU/Linux",
	"chromeos":  "Chrome OS",
}

// importedDevices maps the device classes other analytics tools write onto the
// class Parse stores. A laptop is a desktop-class device to Parse, which reads
// the device from the user agent rather than the screen. Keys are lower case.
var importedDevices = map[string]string{
	"smart tv": DeviceTV,
	"laptop":   DeviceDesktop,
}

// browserNames, systemNames and deviceNames index every name Parse can store by
// its lower-case spelling, so an imported name that differs only in case folds
// onto it.
var (
	browserNames = namesOf(browserRules)
	systemNames  = namesOf(osRules)
	deviceNames  = map[string]string{
		strings.ToLower(DeviceDesktop): DeviceDesktop,
		strings.ToLower(DeviceMobile):  DeviceMobile,
		strings.ToLower(DeviceTablet):  DeviceTablet,
		strings.ToLower(DeviceTV):      DeviceTV,
	}
)

// namesOf collects the names a rule list stores, keyed by lower case.
func namesOf(rules []rule) map[string]string {
	names := make(map[string]string, len(rules))
	for _, r := range rules {
		names[strings.ToLower(r.name)] = r.name
	}

	return names
}

// CanonicalBrowser returns the browser name Parse would store for a browser
// named in imported history. An unrecognised name is returned trimmed and
// otherwise unchanged.
func CanonicalBrowser(name string) string {
	return canonicalName(name, importedBrowsers, browserNames)
}

// CanonicalOS returns the operating system name Parse would store for a system
// named in imported history. An unrecognised name is returned trimmed and
// otherwise unchanged.
func CanonicalOS(name string) string {
	return canonicalName(name, importedSystems, systemNames)
}

// CanonicalDevice returns the device class Parse would store for a device
// class named in imported history. An unrecognised class is returned trimmed
// and otherwise unchanged.
func CanonicalDevice(name string) string {
	return canonicalName(name, importedDevices, deviceNames)
}

// canonicalName looks a name up first among other tools' names and then among
// our own, ignoring case, and returns the trimmed input when neither has it.
func canonicalName(name string, aliases, ours map[string]string) string {
	name = strings.TrimSpace(name)
	key := strings.ToLower(name)

	if canonical, ok := aliases[key]; ok {
		return canonical
	}

	if canonical, ok := ours[key]; ok {
		return canonical
	}

	return name
}

// CanonicalBrowserVersion returns the browser version Parse would store for a
// numeric version taken from imported history: its major number. A version
// that is not dotted digits is returned trimmed and otherwise unchanged.
func CanonicalBrowserVersion(version string) string {
	version = strings.TrimSpace(version)
	if !numericVersion(version) {
		return version
	}

	return majorMinor(version)
}

// CanonicalOSVersion returns the system version Parse would store for a
// numeric version of the named system taken from imported history: the Windows
// release name for a Windows NT number, and at most two components with a
// trailing ".0" dropped for every other system. The system name is the
// canonical one. A version that is not dotted digits is returned trimmed and
// otherwise unchanged.
func CanonicalOSVersion(system, version string) string {
	version = strings.TrimSpace(version)
	if !numericVersion(version) {
		return version
	}

	if system == "Windows" {
		if friendly, ok := windowsNames[version]; ok {
			return friendly
		}

		return version
	}

	return osVersion(system, version)
}

// numericVersion reports whether a version is digits separated by single dots.
func numericVersion(version string) bool {
	if version == "" || version[0] == '.' || version[len(version)-1] == '.' {
		return false
	}

	for i := 0; i < len(version); i++ {
		c := version[i]
		switch {
		case c >= '0' && c <= '9':
		case c == '.' && version[i-1] != '.':
		default:
			return false
		}
	}

	return true
}
