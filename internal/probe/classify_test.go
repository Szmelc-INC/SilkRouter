package probe

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"syscall"
	"testing"
)

func TestClassifyErr(t *testing.T) {
	cases := map[string]error{
		"":              nil,
		ClassTimeout:    fmt.Errorf("request: %w", context.DeadlineExceeded),
		ClassRefused:    &net.OpError{Op: "dial", Err: syscall.ECONNREFUSED},
		ClassReset:      fmt.Errorf("read: %w", syscall.ECONNRESET),
		ClassDNS:        &net.DNSError{Err: "no such host", Name: "x"},
		ClassTLS:        errors.New("tls: handshake failure"),
		ClassAuth:       errors.New("socks5 auth: rejected (status 0x01)"),
		ClassProto:      fmt.Errorf("socks5 connect reply: %w", io.ErrUnexpectedEOF),
		ClassHTTPStatus: errors.New("unexpected response (status 403)"),
		ClassOther:      errors.New("something else"),
	}
	for want, err := range cases {
		if got := ClassifyErr(err); got != want {
			t.Errorf("ClassifyErr(%v) = %q, want %q", err, got, want)
		}
	}
}
