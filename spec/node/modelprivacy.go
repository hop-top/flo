package node

import (
	"net"
	"strings"
)

// ModelPrivacy derives the level of a model URI (node.md §9). The static
// check (V19) and the engine's runtime ceiling both use it.
//
//   - ollama://<model> names the daemon on this machine: local.
//     ollama://<host>[:port]/<model> is local only when the host is; a unix
//     socket (ollama:///path, ollama://unix:/path) is local.
//   - openai://<host>[:port]/<model> is local only when the host is.
//   - every other scheme is hosted (cloud).
//
// A host is local when it is localhost, a *.local / *.localhost name, a
// loopback address (127.0.0.0/8, ::1) or a private one (RFC 1918, fc00::/7).
// ok is false when model is not a URI.
func ModelPrivacy(model string) (Privacy, bool) {
	scheme, rest, ok := strings.Cut(model, "://")
	if !ok || scheme == "" {
		return "", false
	}
	switch scheme {
	case "ollama":
		if !strings.Contains(rest, "/") || isSocket(rest) {
			return PrivacyLocal, true // bare model: the default local daemon
		}
		if IsLocalHost(HostOf(rest)) {
			return PrivacyLocal, true
		}
	case "openai":
		if IsLocalHost(HostOf(rest)) {
			return PrivacyLocal, true
		}
	}
	return PrivacyCloud, true
}

// isSocket reports an authority that is empty (ollama:///run/x.sock) or a
// unix: / percent-encoded path.
func isSocket(rest string) bool {
	return strings.HasPrefix(rest, "/") || strings.HasPrefix(rest, "unix:") || strings.HasPrefix(rest, "%2F") || strings.HasPrefix(rest, "%2f")
}

// HostOf extracts the host of the authority part of rest ("h:8080/path",
// "[::1]:80/x"). Model names after the host may hold characters a URL
// parser refuses ("gemma4:e4b"), so it is cut by hand.
func HostOf(rest string) string {
	auth, _, _ := strings.Cut(rest, "/")
	if strings.HasPrefix(auth, "[") {
		host, _, _ := strings.Cut(strings.TrimPrefix(auth, "["), "]")
		return host
	}
	host, _, _ := strings.Cut(auth, ":")
	return host
}

// IsLocalHost reports a host on this machine or its private network.
func IsLocalHost(h string) bool {
	if h == "" {
		return false
	}
	if h == "localhost" || strings.HasSuffix(h, ".local") || strings.HasSuffix(h, ".localhost") {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && (ip.IsLoopback() || ip.IsPrivate())
}
