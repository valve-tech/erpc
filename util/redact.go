package util

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

func RedactEndpoint(endpoint string) string {
	// Calculate hash of the entire original endpoint
	hasher := sha256.New()
	hasher.Write([]byte(endpoint))
	hash := hex.EncodeToString(hasher.Sum(nil))

	// Parse the endpoint URL
	parsedURL, err := url.Parse(endpoint)
	if err != nil || parsedURL.Scheme == "" || parsedURL.Host == "" {
		// If parsing fails or the URL is incomplete, return just the hash
		return "redacted=" + hash[:5]
	}

	// Construct the redacted endpoint
	var redactedEndpoint string
	if IsNativeProtocol(endpoint) {
		redactedEndpoint = parsedURL.Scheme + "://" + parsedURL.Host + "#redacted=" + hash[:5]
	} else if strings.HasSuffix(parsedURL.Scheme, "envio") {
		redactedEndpoint = parsedURL.Scheme + "://" + parsedURL.Host
	} else if strings.HasSuffix(parsedURL.Scheme, "repository") {
		redactedEndpoint = parsedURL.Scheme + "://" + parsedURL.Host + "#redacted=" + hash[:5]
	} else {
		redactedEndpoint = parsedURL.Scheme + "#redacted=" + hash[:5]
	}

	return redactedEndpoint
}

// RedactedURLPlaceholder stands in for the part of a URL that redaction
// removes. It is visible on purpose: a reader (and a test) can tell a redacted
// URL apart from one that never had a path.
const RedactedURLPlaceholder = "***"

// RedactURL renders an endpoint URL in a form that is safe to show anywhere:
// an error message, a log line, a trace attribute, a metric label or a
// JSON-RPC error returned to a client.
//
// Vendors put the credential in many places: the path (Infura /v3/<key>,
// Alchemy /v2/<key>), the query (dRPC ?dkey=), the userinfo, or the host
// itself for the vendor shorthands (infura://<key>). This function makes no
// guess about which part holds it. It keeps the scheme, and the host only for
// the network schemes that dial a host (http, https, ws, wss, grpc). It drops
// everything else and appends the placeholder when it dropped anything.
//
// An unknown scheme is the safe fallthrough: its host is hidden too, because
// a vendor shorthand carries its key there.
func RedactURL(u *url.URL) string {
	if u == nil {
		return ""
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme == "" || !IsNativeProtocol(scheme+"://") {
		if scheme == "" {
			return RedactedURLPlaceholder
		}
		return scheme + "://" + RedactedURLPlaceholder
	}
	out := scheme + "://" + u.Host
	if u.User != nil || u.Opaque != "" || u.RawQuery != "" || u.ForceQuery ||
		u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		out += "/" + RedactedURLPlaceholder
	}
	return out
}

// RedactURLString is RedactURL for a raw string. A string that does not parse
// as an absolute URL gives no host to keep, so the whole value is hidden.
func RedactURLString(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" {
		if i := strings.Index(raw, "://"); i > 0 {
			return strings.ToLower(raw[:i]) + "://" + RedactedURLPlaceholder
		}
		return RedactedURLPlaceholder
	}
	if u.Host == "" && IsNativeProtocol(strings.ToLower(u.Scheme)+"://") {
		return strings.ToLower(u.Scheme) + "://" + RedactedURLPlaceholder
	}
	return RedactURL(u)
}

// urlInTextPattern finds anything shaped like "scheme://..." up to the first
// character that cannot be part of an unquoted URL. net/http quotes the URL
// with %q, so the closing quote ends the match.
var urlInTextPattern = regexp.MustCompile(`[A-Za-z][A-Za-z0-9+.\-]*://[^\s"'<>` + "`" + `]+`)

// RedactURLsInText redacts every URL inside free text, such as the message of
// an error that a Go library built. It is meant for errors eRPC itself
// produced (net/http, net, crypto/tls, url.Parse), not for a JSON-RPC error an
// upstream returned.
func RedactURLsInText(s string) string {
	if !strings.Contains(s, "://") {
		return s
	}
	return urlInTextPattern.ReplaceAllStringFunc(s, func(m string) string {
		// Prose often closes a URL with punctuation, as in "(via http://x)".
		// Keep that punctuation out of the URL so the host still parses.
		trimmed := strings.TrimRight(m, ").,;:]}")
		return RedactURLString(trimmed) + m[len(trimmed):]
	})
}

// redactedURLError hides every URL in an error message but keeps the original
// error reachable through Unwrap, so errors.Is and errors.As still reach
// sentinels such as context.DeadlineExceeded and concrete types such as
// *url.Error.
type redactedURLError struct {
	cause   error
	message string
}

func (e *redactedURLError) Error() string { return e.message }

func (e *redactedURLError) Unwrap() error { return e.cause }

// RedactURLError returns err with every URL in its message redacted by
// RedactURL. It returns nil for nil, and err itself when the message holds no
// URL.
//
// A *url.Error anywhere in the chain gets special care. Its Error() quotes the
// URL with %q, and url.Parse puts the unparsed input there, which may hold
// spaces or other characters that end a text match early. Its URL field is
// therefore replaced whole before the text scan runs. Each URL in known is
// replaced whole as well, in case a library printed it without a scheme
// boundary the scan can see.
func RedactURLError(err error, known ...*url.URL) error {
	if err == nil {
		return nil
	}
	if _, ok := err.(*redactedURLError); ok {
		return err
	}
	msg := err.Error()
	out := msg
	var ue *url.Error
	if errors.As(err, &ue) && ue.URL != "" {
		red := RedactURLString(ue.URL)
		out = strings.ReplaceAll(out, strconv.Quote(ue.URL), strconv.Quote(red))
		out = strings.ReplaceAll(out, ue.URL, red)
	}
	for _, u := range known {
		if u == nil {
			continue
		}
		raw := u.String()
		if raw == "" {
			continue
		}
		red := RedactURL(u)
		out = strings.ReplaceAll(out, raw, red)
		if u.User != nil {
			// net/http prints the request URL with the password masked, so the
			// raw form above does not match it.
			if _, has := u.User.Password(); has {
				masked := *u
				masked.User = url.UserPassword(u.User.Username(), "***")
				out = strings.ReplaceAll(out, masked.String(), red)
			}
		}
	}
	out = RedactURLsInText(out)
	if out == msg {
		return err
	}
	return &redactedURLError{cause: err, message: out}
}
