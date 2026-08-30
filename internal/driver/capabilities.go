package driver

import (
	"fmt"
	"sort"
)

// Capability names are stable API keys shared by every driver.
const (
	CapabilityAuthQR                = "auth.qr"
	CapabilityAuthSMS               = "auth.sms"
	CapabilityMessagesVisible       = "messages.visible"
	CapabilityMessagesHistory       = "messages.history"
	CapabilityMessagesWatch         = "messages.watch"
	CapabilityMessagesSend          = "messages.send"
	CapabilityAttachmentsSend       = "attachments.send"
	CapabilityOfficialAccountsRead  = "official_accounts.read"
	CapabilityWebOpen               = "web.open"
	CapabilityMiniProgramOpen       = "miniprogram.open"
	CapabilityMiniProgramOpenByName = "miniprogram.open_by_name"
	CapabilitySurfaceAct            = "surface.act"
	CapabilitySurfaceAssetExport    = "surface.assets.export"
)

var knownCapabilities = map[string]struct{}{
	CapabilityAuthQR:                {},
	CapabilityAuthSMS:               {},
	CapabilityMessagesVisible:       {},
	CapabilityMessagesHistory:       {},
	CapabilityMessagesWatch:         {},
	CapabilityMessagesSend:          {},
	CapabilityAttachmentsSend:       {},
	CapabilityOfficialAccountsRead:  {},
	CapabilityWebOpen:               {},
	CapabilityMiniProgramOpen:       {},
	CapabilityMiniProgramOpenByName: {},
	CapabilitySurfaceAct:            {},
	CapabilitySurfaceAssetExport:    {},
}

// CapabilityMap fills absent keys with unsupported. Unknown keys panic because
// they are programmer errors that would otherwise silently fork the public API.
func CapabilityMap(overrides map[string]Support) map[string]Support {
	result := make(map[string]Support, len(knownCapabilities))
	for name := range knownCapabilities {
		result[name] = SupportUnsupported
	}
	for name, support := range overrides {
		if _, ok := knownCapabilities[name]; !ok {
			panic("driver: unknown capability " + name)
		}
		if !validSupport(support) {
			panic("driver: invalid support level for " + name)
		}
		result[name] = support
	}
	return result
}

// ValidateCapabilities is intended for driver contract tests and adapter
// boundaries that need to reject a partial or forked capability map.
func ValidateCapabilities(values map[string]Support) error {
	for name := range knownCapabilities {
		support, ok := values[name]
		if !ok {
			return fmt.Errorf("capability map is missing %q", name)
		}
		if !validSupport(support) {
			return fmt.Errorf("capability %q has invalid support level %q", name, support)
		}
	}
	for name := range values {
		if _, ok := knownCapabilities[name]; !ok {
			return fmt.Errorf("capability map contains unknown key %q", name)
		}
	}
	return nil
}

func CapabilityNames() []string {
	result := make([]string, 0, len(knownCapabilities))
	for name := range knownCapabilities {
		result = append(result, name)
	}
	sort.Strings(result)
	return result
}

func validSupport(value Support) bool {
	switch value {
	case SupportStable, SupportBeta, SupportExperimental, SupportUnsupported:
		return true
	default:
		return false
	}
}
