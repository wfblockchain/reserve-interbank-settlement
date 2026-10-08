// Package anvil starts a local development node for demos and tests.
package anvil

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strconv"
	"time"
)

// Start launches anvil on a free port with its genesis at the given time, so
// a scenario can move the chain's clock forward from there.
func Start(ctx context.Context, bin string, genesis time.Time) (url string, stop func(), err error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", nil, err
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	cmd := exec.CommandContext(ctx, bin, "--port", strconv.Itoa(port),
		"--timestamp", strconv.FormatInt(genesis.Unix(), 10), "--silent")
	if err := cmd.Start(); err != nil {
		return "", nil, err
	}
	stop = func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }
	url = fmt.Sprintf("http://127.0.0.1:%d", port)
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 200*time.Millisecond)
		if err == nil {
			c.Close()
			return url, stop, nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	stop()
	return "", nil, errors.New("anvil did not start")
}

// Bin finds anvil: $ANVIL_BIN, then PATH.
func Bin() (string, error) {
	if b := os.Getenv("ANVIL_BIN"); b != "" {
		return b, nil
	}
	return exec.LookPath("anvil")
}
