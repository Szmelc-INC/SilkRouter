package main

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/szmelc-inc/silkrouter/internal/db"
	"github.com/szmelc-inc/silkrouter/internal/relay"
	"github.com/szmelc-inc/silkrouter/internal/shell"
)

func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	return addr
}

func TestRelayCommand(t *testing.T) {
	sandbox(t)
	for _, k := range []string{shell.EnvActive, shell.EnvSession} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
	up := localProxy(t) // answers generate_204 itself
	addr := freeAddr(t)
	if code, _, _ := runApp(t, "px", "relay", "status", "-l", addr); code != 1 {
		t.Fatal("status without a relay")
	}
	done := make(chan int, 1)
	go func() {
		code, _, errs := runApp(t, "px", "relay", "-x", "-p", up, "-l", addr)
		if code != 0 {
			t.Errorf("relay exited %d: %s", code, errs)
		}
		done <- code
	}()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, ok := relayRunning(context.Background(), addr); ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("relay did not start")
		}
		time.Sleep(50 * time.Millisecond)
	}
	// Traffic through the relay (HTTP proxy side) reaches the upstream.
	u, _ := url.Parse("http://" + addr)
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(u)}, Timeout: 5 * time.Second}
	resp, err := client.Get("http://cp.example.test/generate_204")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("through relay: %s", resp.Status)
	}
	var st relay.Status
	json.Unmarshal([]byte(mustRun(t, "px", "relay", "status", "-l", addr, "--json")), &st)
	if st.Upstream != up || st.Conns < 1 {
		t.Fatalf("status: %+v", st)
	}
	if out := mustRun(t, "px", "relay", "env", "--shell", "zsh", "-l", addr); !strings.Contains(out, "export ALL_PROXY='socks5h://"+addr+"'") ||
		!strings.Contains(out, "export HTTP_PROXY='http://"+addr+"'") {
		t.Fatalf("env:\n%s", out)
	}
	mustRun(t, "px", "relay", "rotate", "-l", addr)
	// px on --relay points the shell at the running relay.
	code, out, errs := runApp(t, "px", "on", "--emit", "bash", "--relay", "--relay-addr", addr)
	if code != 0 || !strings.Contains(out, "export ALL_PROXY='socks5h://"+addr+"'") || !strings.Contains(out, "export https_proxy='http://"+addr+"'") ||
		!strings.Contains(out, "SILKROUTER_METHOD='relay'") {
		t.Fatalf("on --relay: %d\n%s\n%s", code, out, errs)
	}
	mustRun(t, "px", "relay", "stop", "-l", addr)
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("relay did not stop")
	}
	// The stint is a usage row; the tunnel a route measurement.
	var rows []db.Usage
	json.Unmarshal([]byte(mustRun(t, "px", "history", "--mode", "relay", "--json")), &rows)
	if len(rows) != 1 || rows[0].Cmd != "relay" || rows[0].ExitCode == nil || *rows[0].ExitCode != 0 || rows[0].ProxyURL != up {
		t.Fatalf("relay usage: %+v", rows)
	}
	var sh showOutput
	json.Unmarshal([]byte(mustRun(t, "px", "show", up, "--json")), &sh)
	found := false
	for _, m := range sh.Measurements {
		if m.Kind == db.KindRoute && m.Probe == "relay" && m.OK {
			found = true
		}
	}
	if !found || sh.Proxy.UseCount != 1 {
		t.Fatalf("relay facts: %+v / uses %d", sh.Measurements, sh.Proxy.UseCount)
	}
}
