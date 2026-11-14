package mail

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"time"
)

var ErrIMAPUnavailable = errors.New("IMAP temporarily unavailable")
var ErrIMAPCredentials = errors.New("IMAP credentials rejected")

// VerifyICloudCredentials authenticates against the fixed TLS iCloud endpoint.
// It never selects a mailbox, fetches mail, or changes anything in the account.
func VerifyICloudCredentials(ctx context.Context, email, password string) error {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	dialer := tls.Dialer{NetDialer: &net.Dialer{Timeout: 10 * time.Second}, Config: &tls.Config{ServerName: IMAPServer, MinVersion: tls.VersionTLS12}}
	conn, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(IMAPServer, "993"))
	if err != nil {
		return ErrIMAPUnavailable
	}
	defer conn.Close()
	return verifyIMAPConnection(ctx, conn, email, password)
}
func verifyIMAPConnection(ctx context.Context, conn net.Conn, email, password string) error {
	// Closing on cancellation interrupts greeting and authentication.
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	// Keep this authentication-only exchange independent of go-imap's default
	// stderr logger, which can print an upstream response before New returns.
	reader := bufio.NewReaderSize(io.LimitReader(conn, 64<<10), 8192)
	readLine := func() (string, error) {
		line, err := reader.ReadString('\n')
		if err != nil || len(line) > 8192 || !strings.HasSuffix(line, "\r\n") {
			return "", ErrIMAPUnavailable
		}
		return strings.TrimSuffix(line, "\r\n"), nil
	}
	greeting, err := readLine()
	if err != nil || !strings.HasPrefix(strings.ToUpper(greeting), "* OK ") {
		return ErrIMAPUnavailable
	}
	quote := func(value string) (string, error) {
		if strings.ContainsAny(value, "\r\n\x00") {
			return "", ErrIMAPCredentials
		}
		value = strings.ReplaceAll(value, `\`, `\\`)
		return `"` + strings.ReplaceAll(value, `"`, `\"`) + `"`, nil
	}
	user, err := quote(email)
	if err != nil {
		return err
	}
	pass, err := quote(password)
	if err != nil {
		return err
	}
	if _, err = fmt.Fprintf(conn, "HME1 LOGIN %s %s\r\n", user, pass); err != nil {
		return ErrIMAPUnavailable
	}
	for i := 0; i < 64; i++ {
		line, err := readLine()
		if err != nil {
			return ErrIMAPUnavailable
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return ErrIMAPUnavailable
		}
		if fields[0] == "*" {
			if strings.EqualFold(fields[1], "BYE") {
				return ErrIMAPUnavailable
			}
			continue
		}
		if fields[0] != "HME1" {
			return ErrIMAPUnavailable
		}
		switch strings.ToUpper(fields[1]) {
		case "OK":
			if ctx.Err() != nil {
				return ErrIMAPUnavailable
			}
			return nil
		case "NO", "BAD":
			return ErrIMAPCredentials
		default:
			return ErrIMAPUnavailable
		}
	}
	return ErrIMAPUnavailable
}
