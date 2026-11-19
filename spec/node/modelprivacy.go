package node

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
)

// ErrNotModelURI is returned for a string that is not scheme://....
var ErrNotModelURI = errors.New("not a model URI")

// ErrNotRunnable is returned for a model URI form the model client cannot
// run (kit go/ai/llm).
var ErrNotRunnable = errors.New("model URI not runnable by the model client")

const runnableHint = "write ollama://<model> for the local daemon or scheme://host:port/<model>"

// ModelPrivacy derives the level of a model URI (node.md §9). The static
// check (V19) and the engine's runtime ceiling both use it. It follows the
// model client's endpoint precedence (kit go/ai/llm/schemes.go:296-311):
//
//  1. the base_url query param: the locality of its host;
//  2. else the URI's host, in the host:port/<model> form;
//  3. else the scheme's declared default endpoint (defaultLocal).
//
// A host is local when it is localhost, a loopback
// address (127.0.0.0/8, ::1), a private one (RFC 1918, fc00::/7), or listed
// in local (privacy.local_hosts). Tailnet addresses (100.64.0.0/10,
// *.ts.net) and every other named host are cloud unless listed.
//
// Forms the model client cannot run return ErrNotRunnable with guidance:
// an empty authority (ollama:///x), a unix socket, a portless host
// (ollama://localhost/x reads there as the model "localhost/x" on the
// scheme's default endpoint), and a non-numeric port. A string without
// scheme:// returns ErrNotModelURI. A nil local lists nothing.
func ModelPrivacy(model string, local *LocalHosts) (Privacy, error) {
	scheme, rest, ok := strings.Cut(model, "://")
	if !ok || scheme == "" {
		return "", ErrNotModelURI
	}
	var params map[string]string
	if q := strings.Index(rest, "?"); q >= 0 {
		params = parseQuery(rest[q+1:])
		rest = rest[:q]
	}
	host, err := authorityHost(model, rest)
	if err != nil {
		return "", err
	}
	if v, ok := params["base_url"]; ok { // an empty one drops the host (kit schemes.go:303-305)
		host = ""
		if v != "" {
			bh, err := baseURLHost(v)
			if err != nil {
				return "", fmt.Errorf("%w: %q: base_url: %v; %s", ErrNotRunnable, model, err, runnableHint)
			}
			return levelOf(bh, local), nil
		}
	}
	if host != "" {
		return levelOf(host, local), nil
	}
	if defaultLocal[scheme] {
		return PrivacyLocal, nil
	}
	return PrivacyCloud, nil
}

// defaultLocal lists the schemes whose declared default endpoint is on this
// machine. Every other scheme reaches a hosted endpoint by default: openai,
// openrouter, xai, groq, together, fireworks, deepseek, mistral (kit
// go/ai/llm/openai/openai.go:57-66, openai's URL for the rest) and anthropic
// (anthropic/anthropic.go:31). A scheme not listed is cloud.
var defaultLocal = map[string]bool{
	"ollama":   true, // http://localhost:11434, ollama/ollama.go:27
	"lmstudio": true, // http://localhost:1234/v1, openai/openai.go:66
}

// authorityHost reads the host of rest (what follows scheme://, before any
// query) as kit's ParseURI does (go/ai/llm/config.go:126-138): the first
// segment is a host only when a slash follows it and it is host:port;
// anything else is the model name, on the scheme's default endpoint. It is
// stricter than kit in refusing what kit would run as a model name that no
// one means: see ErrNotRunnable.
func authorityHost(model, rest string) (string, error) {
	refuse := func(why string) error {
		return fmt.Errorf("%w: %q: %s; %s", ErrNotRunnable, model, why, runnableHint)
	}
	if strings.HasPrefix(rest, "/") {
		return "", refuse("empty authority or unix socket")
	}
	if lr := strings.ToLower(rest); strings.HasPrefix(lr, "unix:") || strings.HasPrefix(lr, "%2f") {
		return "", refuse("unix socket")
	}
	slash := strings.Index(rest, "/")
	if slash < 0 {
		return "", nil // a model name
	}
	cand := rest[:slash]
	if h, p, err := net.SplitHostPort(cand); err == nil && p != "" {
		if _, err := strconv.ParseUint(p, 10, 16); err != nil {
			return "", refuse("port " + p + " is not a number")
		}
		return h, nil
	}
	// No port: kit reads the whole rest as a model name. A localhost or an
	// address there is a host the user meant, which would not be reached.
	if strings.EqualFold(cand, "localhost") || net.ParseIP(strings.Trim(cand, "[]")) != nil {
		return "", refuse("host " + cand + " has no port")
	}
	return "", nil
}

// parseQuery splits k=v&k2=v2 as kit does (go/ai/llm/config.go:150-163).
func parseQuery(q string) map[string]string {
	m := map[string]string{}
	for _, pair := range strings.Split(q, "&") {
		k, v, _ := strings.Cut(pair, "=")
		if k != "" {
			m[k] = v
		}
	}
	return m
}

func baseURLHost(v string) (string, error) {
	u, err := url.Parse(v)
	if err != nil {
		return "", err
	}
	if u.Scheme != "http" && u.Scheme != "https" || u.Hostname() == "" {
		return "", errors.New("not an http(s) URL with a host")
	}
	return u.Hostname(), nil
}

func levelOf(host string, local *LocalHosts) Privacy {
	if IsLocalHost(host, local) {
		return PrivacyLocal
	}
	return PrivacyCloud
}

// IsLocalHost reports a host on this machine or its private network, or one
// the operator listed.
func IsLocalHost(h string, local *LocalHosts) bool {
	h = strings.ToLower(strings.TrimSuffix(h, "."))
	if h == "" {
		return false
	}
	if h == "localhost" {
		return true
	}
	ip := net.ParseIP(h)
	if ip != nil && (ip.IsLoopback() || ip.IsPrivate()) {
		return true
	}
	return local.has(h, ip)
}

// LocalHosts is the operator's list of hosts to treat as local
// (privacy.local_hosts): names and CIDRs (a bare address is a /32 or /128).
type LocalHosts struct {
	names map[string]bool
	nets  []*net.IPNet
}

// NewLocalHosts parses entries; an entry with a slash must be a CIDR.
func NewLocalHosts(entries []string) (*LocalHosts, error) {
	l := &LocalHosts{names: map[string]bool{}}
	for _, e := range entries {
		e = strings.ToLower(strings.TrimSpace(e))
		switch {
		case e == "":
		case strings.Contains(e, "/"):
			_, n, err := net.ParseCIDR(e)
			if err != nil {
				return nil, fmt.Errorf("local host %q: not a CIDR", e)
			}
			l.nets = append(l.nets, n)
		default:
			l.names[strings.TrimSuffix(e, ".")] = true
		}
	}
	return l, nil
}

func (l *LocalHosts) has(name string, ip net.IP) bool {
	if l == nil {
		return false
	}
	if l.names[name] {
		return true
	}
	if ip == nil {
		return false
	}
	for _, n := range l.nets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}
