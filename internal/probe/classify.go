package probe

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"os"
	"strings"
	"syscall"
)

// Error classes, the err_class vocabulary of the database (see
// docs/ARCHITECTURE-v4.md). Kept as plain strings so this package does not
// depend on internal/db.
const (
	ClassTimeout    = "timeout"
	ClassRefused    = "refused"
	ClassReset      = "reset"
	ClassDNS        = "dns"
	ClassTLS        = "tls"
	ClassAuth       = "auth"
	ClassProto      = "proto"
	ClassHTTPStatus = "http_status"
	ClassOther      = "other"
)

// ClassifyErr maps a probe / dial error to its err_class ("" for nil).
func ClassifyErr(err error) string {
	if err == nil {
		return ""
	}
	var dnsErr *net.DNSError
	var certErr *x509.CertificateInvalidError
	var unkAuth x509.UnknownAuthorityError
	var hostErr x509.HostnameError
	var recErr tls.RecordHeaderError
	var alert tls.AlertError
	switch {
	case errors.Is(err, context.DeadlineExceeded) || errors.Is(err, os.ErrDeadlineExceeded):
		return ClassTimeout
	case errors.Is(err, syscall.ECONNREFUSED):
		return ClassRefused
	case errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE) || errors.Is(err, syscall.ECONNABORTED):
		return ClassReset
	case errors.As(err, &dnsErr):
		return ClassDNS
	case errors.As(err, &certErr) || errors.As(err, &unkAuth) || errors.As(err, &hostErr) ||
		errors.As(err, &recErr) || errors.As(err, &alert):
		return ClassTLS
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return ClassTimeout
	}
	msg := strings.ToLower(err.Error())
	switch {
	case containsAny(msg, "timeout", "timed out", "deadline exceeded"):
		return ClassTimeout
	case containsAny(msg, "connection refused", "actively refused"):
		return ClassRefused
	case containsAny(msg, "connection reset", "broken pipe", "forcibly closed"):
		return ClassReset
	case containsAny(msg, "no such host", "cannot resolve", "server misbehaving"):
		return ClassDNS
	case containsAny(msg, "tls", "x509", "certificate"):
		return ClassTLS
	case containsAny(msg, "auth", "407", "credential"):
		return ClassAuth
	case strings.Contains(msg, "unexpected response (status"):
		return ClassHTTPStatus
	case errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) ||
		containsAny(msg, "socks", "bad version", "malformed", "eof", "proxyconnect", "bad reply"):
		return ClassProto
	}
	return ClassOther
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}
