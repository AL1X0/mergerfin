package upstream

import (
	"context"
	"fmt"
	"strings"
)

type sessionIdentityKey struct{}

// SessionIdentity carries downstream MergerFin user and client metadata so upstream
// Jellyfin servers can attribute active sessions and devices to the real MergerFin user.
type SessionIdentity struct {
	ProxyUsername string
	ClientName    string
	DeviceName    string
	DeviceID      string
	Version       string
	IsMappedUser  bool
}

// WithSessionIdentity attaches a SessionIdentity to ctx for upstream requests.
func WithSessionIdentity(ctx context.Context, id SessionIdentity) context.Context {
	return context.WithValue(ctx, sessionIdentityKey{}, id)
}

// SessionIdentityFromContext extracts SessionIdentity from ctx, or returns a zero value if absent.
func SessionIdentityFromContext(ctx context.Context) SessionIdentity {
	if ctx == nil {
		return SessionIdentity{}
	}
	if id, ok := ctx.Value(sessionIdentityKey{}).(SessionIdentity); ok {
		return id
	}
	return SessionIdentity{}
}

// FormatUpstreamDeviceID builds a stable, ASCII-safe DeviceId for upstream Jellyfin
// sessions so multiple MergerFin users on the same client do not collide.
func FormatUpstreamDeviceID(id SessionIdentity) string {
	user := sanitizeDeviceToken(id.ProxyUsername, 24)
	dev := sanitizeDeviceToken(id.DeviceID, 36)
	switch {
	case user != "" && dev != "":
		return "mf-" + user + "-" + dev
	case user != "":
		return "mf-" + user
	case dev != "":
		return "mf-" + dev
	default:
		return "mergerfin-proxy"
	}
}

// BuildMediaBrowserHeader formats a Jellyfin Authorization header carrying the
// access token and (when present) the real MergerFin user and client metadata.
func BuildMediaBrowserHeader(id SessionIdentity, accessToken string) string {
	user := sanitizeHeaderAttr(id.ProxyUsername, 48)
	client := sanitizeHeaderAttr(id.ClientName, 48)
	device := sanitizeHeaderAttr(id.DeviceName, 64)
	version := sanitizeHeaderAttr(id.Version, 24)

	// Background/system calls without user or client context keep the default proxy header.
	if user == "" && client == "" && device == "" && strings.TrimSpace(id.DeviceID) == "" {
		if accessToken == "" {
			return jellyfinAuthHeader
		}
		return fmt.Sprintf(`%s, Token=%q`, jellyfinAuthHeader, accessToken)
	}

	if client == "" {
		client = "MergerFin"
	}
	if version == "" {
		version = "12.4.0"
	}

	// When sharing a single upstream account (not mapped to a dedicated upstream user),
	// include the MergerFin username in Device so upstream admins immediately see who is watching.
	var displayDevice string
	if !id.IsMappedUser && user != "" {
		if device != "" {
			displayDevice = fmt.Sprintf("%s (%s)", user, device)
		} else {
			displayDevice = user
		}
	} else {
		if device != "" {
			displayDevice = device
		} else {
			displayDevice = "MergerFin"
		}
	}

	deviceID := FormatUpstreamDeviceID(id)
	header := fmt.Sprintf(
		`MediaBrowser Client=%q, Device=%q, DeviceId=%q, Version=%q`,
		client, displayDevice, deviceID, version,
	)
	if accessToken != "" {
		header += fmt.Sprintf(`, Token=%q`, accessToken)
	}
	return header
}

func sanitizeHeaderAttr(raw string, maxLen int) string {
	var b strings.Builder
	b.Grow(len(raw))
	for _, r := range strings.TrimSpace(raw) {
		if r < 0x20 || r == 0x7f || r == '"' || r == '\\' {
			continue
		}
		b.WriteRune(r)
		if maxLen > 0 && b.Len() >= maxLen {
			break
		}
	}
	return strings.TrimSpace(b.String())
}

func sanitizeDeviceToken(raw string, maxLen int) string {
	var b strings.Builder
	b.Grow(len(raw))
	for _, r := range strings.ToLower(strings.TrimSpace(raw)) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			b.WriteRune(r)
			if maxLen > 0 && b.Len() >= maxLen {
				break
			}
		}
	}
	return strings.Trim(b.String(), "-_")
}
