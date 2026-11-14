package mail

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"
)

func TestVerifyIMAPAuthenticationOnly(t *testing.T) {
	for _, tc := range []struct {
		name, response string
		want           error
	}{
		{"accepted", "HME1 OK authenticated\r\n", nil},
		{"rejected", "HME1 NO [AUTHENTICATIONFAILED] synthetic-secret\r\n", ErrIMAPCredentials},
		{"bye", "* BYE synthetic-secret\r\n", ErrIMAPUnavailable},
		{"malformed", "unexpected synthetic-secret\r\n", ErrIMAPUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			local, remote := net.Pipe()
			defer local.Close()
			commands := make(chan string, 1)
			done := make(chan struct{})
			go func() {
				defer close(done)
				defer remote.Close()
				fmt.Fprint(remote, "* OK synthetic IMAP\r\n")
				r := bufio.NewReader(remote)
				line, _ := r.ReadString('\n')
				commands <- line
				fmt.Fprint(remote, tc.response)
				extra, _ := r.ReadString('\n')
				if extra != "" {
					t.Error("unexpected command")
				}
			}()
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			err := verifyIMAPConnection(ctx, local, "owner@icloud.com", "abcd-efgh-ijkl-mnop")
			local.Close()
			<-done
			if err != tc.want {
				t.Fatal(err)
			}
			command := <-commands
			if command != "HME1 LOGIN \"owner@icloud.com\" \"abcd-efgh-ijkl-mnop\"\r\n" {
				t.Fatal("not a LOGIN-only exchange")
			}
		})
	}
}
func TestVerifyIMAPCancellationAndUntrustedGreeting(t *testing.T) {
	for _, greeting := range []string{"", "* PREAUTH bypass\r\n", "* OK " + strings.Repeat("x", 9000) + "\r\n"} {
		local, remote := net.Pipe()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
		done := make(chan struct{})
		go func() {
			defer close(done)
			defer remote.Close()
			if greeting != "" {
				fmt.Fprint(remote, greeting)
			}
			buf := make([]byte, 1)
			remote.Read(buf)
		}()
		if e := verifyIMAPConnection(ctx, local, "owner@icloud.com", "password"); e != ErrIMAPUnavailable {
			t.Fatal(e)
		}
		cancel()
		local.Close()
		<-done
	}
}
