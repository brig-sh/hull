# Copyright (c) 2026, NOFire AI
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http:#www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# Versioning variables
COMMIT         := $(shell git describe --dirty --long --always 2>/dev/null || echo dev)
VERSION        := $(shell cat $(CURDIR)/VERSION 2>/dev/null || echo 0.0.0)-$(COMMIT)

# Path variables
#? BUILD_DIR Directory to place produced binaries (default: ${CWD}/dist)
BUILD_DIR      ?= ${CURDIR}/dist
#? PREFIX Directory to install binaries (default: /usr/local/bin)
PREFIX         ?= /usr/local/bin

# Architecture (Apple Silicon only for now)
ARCH           ?= arm64

# Binary variables
HULL_BIN := $(BUILD_DIR)/hull
VZ_RUNNER_DIR   := $(CURDIR)/vz-runner
VZ_RUNNER_BIN   := $(VZ_RUNNER_DIR)/.build/arm64-apple-macosx/release/vz-runner

# Golang variables
GO             ?= go
LDFLAGS        := -X main.version=$(VERSION) -s -w
# Telemetry ingestion endpoint (NOFireAI/engineering#1002). Left empty
# (dev builds), the client never sends anything.
ifneq ($(TELEMETRY_ENDPOINT),)
LDFLAGS        += -X github.com/brig-sh/hull/internal/telemetry.Endpoint=$(TELEMETRY_ENDPOINT)
endif

# macOS code-signing
#
# vz-runner must be signed with the com.apple.security.virtualization
# entitlement. Signing with a real Apple identity (Apple Development or
# Developer ID) lets the entitlement be honored with SIP enabled; ad-hoc
# signing ("-") only works when AMFI is disabled. NAT networking needs no
# extra entitlement.
#
#? CODESIGN_IDENTITY Signing identity (default: "-" ad-hoc). Use "Developer ID Application: ..." for distribution.
CODESIGN_IDENTITY ?= -
#? VZ_ENTITLEMENTS Entitlements plist for vz-runner (default: virtualization only, no vmnet)
VZ_ENTITLEMENTS   ?= $(VZ_RUNNER_DIR)/Entitlements-novmnet.plist
#? CODESIGN_KEYCHAIN Keychain holding the identity. Pin it when other jobs may
#?     share this login: `security list-keychains -d user -s` is a per-user
#?     setting, so a concurrent build that sets its own search list evicts this
#?     one and codesign fails with errSecInternalComponent mid-run.
CODESIGN_KEYCHAIN ?=
CODESIGN_FLAGS    := --force --options runtime $(if $(CODESIGN_KEYCHAIN),--keychain "$(CODESIGN_KEYCHAIN)")
#? NOTARY_PROFILE notarytool keychain profile created with `xcrun notarytool store-credentials`
NOTARY_PROFILE    ?= urunc-notary
DMG               := $(BUILD_DIR)/hull.dmg
RELEASE_VERSION   := $(shell cat $(CURDIR)/VERSION 2>/dev/null || echo 0.0.0)
TARBALL           := $(BUILD_DIR)/hull-$(RELEASE_VERSION)-arm64.tar.gz

## default Build and sign hull + vz-runner.
.PHONY: default
default: macos

## urunc_macos Build the hull CLI.
.PHONY: urunc_macos
urunc_macos:
	mkdir -p $(BUILD_DIR)
	GOARCH=$(ARCH) $(GO) build -ldflags "$(LDFLAGS)" \
		-o $(HULL_BIN)_$(ARCH) $(CURDIR)/cmd/hull

## vz_runner Build the Swift Virtualization.framework runner.
.PHONY: vz_runner
vz_runner:
	cd $(VZ_RUNNER_DIR) && swift build -c release

## sign Code-sign hull + vz-runner and strip quarantine.
##      Override the identity: make sign CODESIGN_IDENTITY="Apple Development: Name (TEAMID)"
.PHONY: sign
sign:
	codesign $(CODESIGN_FLAGS) --sign "$(CODESIGN_IDENTITY)" \
		--entitlements $(VZ_ENTITLEMENTS) $(VZ_RUNNER_BIN)
	codesign $(CODESIGN_FLAGS) --sign "$(CODESIGN_IDENTITY)" $(HULL_BIN)_$(ARCH)
	xattr -cr $(VZ_RUNNER_BIN) $(HULL_BIN)_$(ARCH)
	@echo "Signed vz-runner + hull with identity: $(CODESIGN_IDENTITY)"

## codesign_verify Print signature authority + entitlements of the signed binaries.
.PHONY: codesign_verify
codesign_verify:
	@echo "== hull =="; codesign -dvvv $(HULL_BIN)_$(ARCH) 2>&1 | grep -E "Authority|flags" || true
	@echo "== vz-runner =="; codesign -dvvv $(VZ_RUNNER_BIN) 2>&1 | grep -E "Authority|flags" || true
	@codesign -d --entitlements - --xml $(VZ_RUNNER_BIN) 2>/dev/null && echo || true

## test Run the Go unit + conformance suite.
.PHONY: test
test:
	$(GO) test -count=1 ./...

## conformance-report Regenerate docs/compose-conformance.md from the manifest.
.PHONY: conformance-report
conformance-report:
	$(GO) run ./test/conformance/cmd/report

## macos Build hull + vz-runner, then sign both.
.PHONY: macos
macos: urunc_macos vz_runner sign

APP := $(BUILD_DIR)/hull.app

## app Build the hull.app bundle (binaries + icon), signed.
##     vz-runner sits next to hull inside Contents/MacOS, so the
##     executable-sibling discovery keeps working from /Applications.
.PHONY: app
app: urunc_macos vz_runner
	rm -rf $(APP)
	mkdir -p $(APP)/Contents/MacOS $(APP)/Contents/Resources
	cp packaging/Info.plist $(APP)/Contents/Info.plist
	cp -p $(HULL_BIN)_$(ARCH) $(APP)/Contents/MacOS/hull
	cp -p $(VZ_RUNNER_BIN) $(APP)/Contents/MacOS/vz-runner
	cp packaging/AppIcon.icns $(APP)/Contents/Resources/AppIcon.icns
	# Sign nested code first (vz-runner carries the virtualization
	# entitlement), then the bundle itself — never --deep.
	codesign $(CODESIGN_FLAGS) --sign "$(CODESIGN_IDENTITY)" \
		--entitlements $(VZ_ENTITLEMENTS) $(APP)/Contents/MacOS/vz-runner
	codesign $(CODESIGN_FLAGS) --sign "$(CODESIGN_IDENTITY)" $(APP)/Contents/MacOS/hull
	codesign $(CODESIGN_FLAGS) --sign "$(CODESIGN_IDENTITY)" $(APP)
	xattr -cr $(APP)
	@echo "Built $(APP)"

## dmg_image Build and sign the drag-to-Applications installer image (no
##     notarization) — the shared step for local dmg and CI. Needs create-dmg.
.PHONY: dmg_image
dmg_image: app
	rm -rf $(BUILD_DIR)/dmg-stage $(DMG)
	mkdir -p $(BUILD_DIR)/dmg-stage
	cp -R $(APP) $(BUILD_DIR)/dmg-stage/
	create-dmg \
		--volname "urunc" \
		--volicon packaging/VolumeIcon.icns \
		--background packaging/dmg-background.tiff \
		--window-size 660 420 \
		--icon-size 128 \
		--icon "hull.app" 180 280 \
		--hide-extension "hull.app" \
		--app-drop-link 480 280 \
		--no-internet-enable \
		$(DMG) $(BUILD_DIR)/dmg-stage/
	codesign --force --sign "$(CODESIGN_IDENTITY)" $(DMG)
	@echo "Built + signed: $(DMG)"

## release_tarball Package the SIGNED binaries out of the app bundle into the
##     Homebrew release tarball + sha256. Deliberately does NOT depend on
##     `app`: it must run after the app was built (and the dmg notarized).
##
##     The bundle's main executable (hull, = CFBundleExecutable) is
##     sealed to the bundle's Info.plist + _CodeSignature. Extracted as a lone
##     binary its signature is INVALID ("invalid Info.plist / resource
##     directory"), so AMFI SIGKILLs it at exec ("Killed: 9") on any Mac that
##     enforces code signing. We therefore re-sign both staged binaries as
##     *standalone* Developer ID code; vz-runner keeps its virtualization
##     entitlement. A --verify --strict gate fails the build if either lone
##     binary is ever invalid again.
.PHONY: release_tarball
release_tarball:
	@case "$(CODESIGN_IDENTITY)" in \
		"Developer ID Application:"*) ;; \
		*) test "$(ALLOW_ADHOC_TARBALL)" = 1 || { \
			echo "error: release_tarball signs the binaries the Homebrew tap installs," >&2; \
			echo "       so it needs a Developer ID identity, not '$(CODESIGN_IDENTITY)'." >&2; \
			echo "       An ad-hoc signature passes codesign --verify, but macOS will not" >&2; \
			echo "       honor vz-runner's virtualization entitlement under SIP, so Vz VMs" >&2; \
			echo "       fail to start for anyone who installed from the tap." >&2; \
			echo "       Pass CODESIGN_IDENTITY=\"Developer ID Application: ...\", or set" >&2; \
			echo "       ALLOW_ADHOC_TARBALL=1 to package an unshippable one on purpose." >&2; \
			exit 1; }; ;; \
	esac
	@test -x $(APP)/Contents/MacOS/hull -a -x $(APP)/Contents/MacOS/vz-runner || \
		{ echo "error: $(APP) not built — run 'make dmg_image' (or 'make app') first" >&2; exit 1; }
	rm -rf $(BUILD_DIR)/tarball-stage $(TARBALL) $(TARBALL).sha256
	mkdir -p $(BUILD_DIR)/tarball-stage
	cp -p $(APP)/Contents/MacOS/hull $(APP)/Contents/MacOS/vz-runner $(BUILD_DIR)/tarball-stage/
	codesign $(CODESIGN_FLAGS) --sign "$(CODESIGN_IDENTITY)" \
		--entitlements $(VZ_ENTITLEMENTS) $(BUILD_DIR)/tarball-stage/vz-runner
	codesign $(CODESIGN_FLAGS) --sign "$(CODESIGN_IDENTITY)" $(BUILD_DIR)/tarball-stage/hull
	codesign --verify --strict $(BUILD_DIR)/tarball-stage/hull
	codesign --verify --strict $(BUILD_DIR)/tarball-stage/vz-runner
	@# --verify checks the seal, not the signer, and the entitlement is embedded
	@# either way -- so neither notices an ad-hoc signature. Assert the identity.
	@for b in hull vz-runner; do \
		codesign -dvv $(BUILD_DIR)/tarball-stage/$$b 2>&1 \
			| grep -q "Authority=Developer ID Application" \
			|| { test "$(ALLOW_ADHOC_TARBALL)" = 1 \
				|| { echo "error: $$b is not Developer ID signed" >&2; exit 1; }; }; \
	done
	cp LICENSE $(BUILD_DIR)/tarball-stage/
	tar -C $(BUILD_DIR)/tarball-stage -czf $(TARBALL) hull vz-runner LICENSE
	(cd $(BUILD_DIR) && shasum -a 256 $(notdir $(TARBALL)) | tee $(notdir $(TARBALL)).sha256)
	@echo "Built $(TARBALL)"

## dmg dmg_image + notarize + staple, via the $(NOTARY_PROFILE) keychain
##     profile. CI notarizes with an API key instead (see sign-notarize.yml):
##     make dmg CODESIGN_IDENTITY="Developer ID Application: Name (TEAMID)"
.PHONY: dmg
dmg: dmg_image
	xcrun notarytool submit $(DMG) --keychain-profile $(NOTARY_PROFILE) --wait
	xcrun stapler staple $(DMG)
	xcrun stapler validate $(DMG)
	@echo "Built + notarized + stapled: $(DMG)"

## install Install hull and vz-runner side by side in PREFIX.
##         vz-runner is discovered next to hull, so both must be copied.
.PHONY: install
install: macos
	install -m0755 $(HULL_BIN)_$(ARCH) $(PREFIX)/hull
	install -m0755 $(VZ_RUNNER_BIN) $(PREFIX)/vz-runner
	codesign $(CODESIGN_FLAGS) --sign "$(CODESIGN_IDENTITY)" \
		--entitlements $(VZ_ENTITLEMENTS) $(PREFIX)/vz-runner

## clean Remove build artifacts.
.PHONY: clean
clean:
	rm -rf $(BUILD_DIR) $(VZ_RUNNER_DIR)/.build

## help Show this help message
help:
	@echo 'Usage: make <target> <flags>'
	@echo 'Targets:'
	@grep -w "^##" $(MAKEFILE_LIST) | sed -n 's/^## /\t/p'
	@echo 'Flags:'
	@grep -w "^#?" $(MAKEFILE_LIST) | sed -n 's/^#? /\t/p'
