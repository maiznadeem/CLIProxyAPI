package auth

import (
	"strings"
	"sync"

	log "github.com/sirupsen/logrus"
)

const (
	// AttributeRoutingMode is the auth attribute and auth file JSON field holding the manual routing mode.
	AttributeRoutingMode = "routing_mode"

	// RoutingModeNormal leaves a credential in regular rotation (default).
	RoutingModeNormal = "normal"
	// RoutingModePreserve keeps all traffic away from a credential.
	RoutingModePreserve = "preserve"
	// RoutingModeFocus concentrates traffic on a credential while it is available.
	RoutingModeFocus = "focus"
)

// NormalizeRoutingMode trims and lowercases a routing mode; unknown values map to normal.
func NormalizeRoutingMode(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case RoutingModePreserve:
		return RoutingModePreserve
	case RoutingModeFocus:
		return RoutingModeFocus
	default:
		return RoutingModeNormal
	}
}

// IsValidRoutingMode reports whether raw names one of the supported routing modes.
func IsValidRoutingMode(raw string) bool {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case RoutingModeNormal, RoutingModePreserve, RoutingModeFocus:
		return true
	default:
		return false
	}
}

// RoutingMode returns the normalized routing mode stored in the auth attributes.
func RoutingMode(auth *Auth) string {
	if auth == nil || len(auth.Attributes) == 0 {
		return RoutingModeNormal
	}
	raw, ok := auth.Attributes[AttributeRoutingMode]
	if !ok {
		return RoutingModeNormal
	}
	return NormalizeRoutingMode(raw)
}

// ApplyAuthRoutingModeMetadata copies a non-normal file routing_mode into the auth's metadata and
// routing attributes. It reports whether a mode was applied; normal or invalid values clear the attribute.
func ApplyAuthRoutingModeMetadata(auth *Auth, metadata map[string]any) bool {
	if auth == nil {
		return false
	}
	delete(auth.Attributes, AttributeRoutingMode)
	raw, ok := metadata[AttributeRoutingMode].(string)
	if !ok {
		return false
	}
	mode := NormalizeRoutingMode(raw)
	if mode == RoutingModeNormal {
		return false
	}
	if auth.Metadata == nil {
		auth.Metadata = make(map[string]any)
	}
	auth.Metadata[AttributeRoutingMode] = raw
	if auth.Attributes == nil {
		auth.Attributes = make(map[string]string)
	}
	auth.Attributes[AttributeRoutingMode] = mode
	return true
}

// preserveSkipLogged remembers credentials whose preserved status was already logged.
var preserveSkipLogged sync.Map

func logPreservedSkip(auth *Auth) {
	if auth == nil || auth.ID == "" {
		return
	}
	if _, loaded := preserveSkipLogged.LoadOrStore(auth.ID, struct{}{}); !loaded {
		log.Infof("credential %s is in preserve routing mode and is skipped by scheduling", auth.ID)
	}
}

// excludePreservedAuths drops preserved credentials. The input slice is returned unchanged when
// no credential is preserved, so the common case allocates nothing.
func excludePreservedAuths(auths []*Auth) []*Auth {
	firstPreserved := -1
	for i, auth := range auths {
		if RoutingMode(auth) == RoutingModePreserve {
			firstPreserved = i
			break
		}
	}
	if firstPreserved < 0 {
		return auths
	}
	kept := make([]*Auth, 0, len(auths)-1)
	kept = append(kept, auths[:firstPreserved]...)
	logPreservedSkip(auths[firstPreserved])
	for _, auth := range auths[firstPreserved+1:] {
		if RoutingMode(auth) == RoutingModePreserve {
			logPreservedSkip(auth)
			continue
		}
		kept = append(kept, auth)
	}
	return kept
}

// restrictToFocusedAuths narrows already-available credentials to the focused ones. When none is
// focused the input is returned unchanged, so focus never causes an outage.
func restrictToFocusedAuths(auths []*Auth) []*Auth {
	focused := 0
	for _, auth := range auths {
		if RoutingMode(auth) == RoutingModeFocus {
			focused++
		}
	}
	if focused == 0 || focused == len(auths) {
		return auths
	}
	out := make([]*Auth, 0, focused)
	for _, auth := range auths {
		if RoutingMode(auth) == RoutingModeFocus {
			out = append(out, auth)
		}
	}
	return out
}

// applyRoutingModes applies preserve exclusion and focus restriction to available credentials.
func applyRoutingModes(auths []*Auth) []*Auth {
	return restrictToFocusedAuths(excludePreservedAuths(auths))
}
