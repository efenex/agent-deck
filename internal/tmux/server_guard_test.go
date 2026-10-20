package tmux

import (
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/testutil"
)

// Tests for the 2026-10-20 socket-steal guard (server_guard.go). Every tmux
// server here runs on an isolated `-S <tmpdir>/s` socket, and every one is
// killed BY PID in cleanup: once a test deletes the socket file,
// KillTmuxServersUnder can no longer reach the server, and a path-based
// cleanup would leak it (the hang report found seven such leaks).

// guardTestEnv makes the guard fast and observable, and restores everything.
func guardTestEnv(t *testing.T) *[]ServerRecoveryEvent {
	t.Helper()
	var mu sync.Mutex
	events := &[]ServerRecoveryEvent{}
	prevBackoff, prevSleep, prevSettle, prevNotify := serverReachBackoff, guardSleep, usr1Settle, notifyServerEvent
	prevList, prevSignal, prevAsync := listTmuxServers, signalServer, runRecoveryAsync
	serverReachBackoff = []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond, time.Millisecond, time.Millisecond}
	guardSleep = func(time.Duration) { time.Sleep(5 * time.Millisecond) }
	usr1Settle = time.Second
	notifyServerEvent = func(ev ServerRecoveryEvent) {
		mu.Lock()
		*events = append(*events, ev)
		mu.Unlock()
	}
	runRecoveryAsync = func(fn func()) { fn() }
	resetServerGuardState()
	t.Cleanup(func() {
		serverReachBackoff, guardSleep, usr1Settle, notifyServerEvent = prevBackoff, prevSleep, prevSettle, prevNotify
		listTmuxServers, signalServer, runRecoveryAsync = prevList, prevSignal, prevAsync
		resetServerGuardState()
	})
	return events
}

// shortTempDir fits sun_path (104 bytes on darwin), which t.TempDir under
// /var/folders does not.
func shortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "sg-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func tmuxNoEnv(args ...string) *exec.Cmd {
	cmd := exec.Command("tmux", args...)
	cmd.Env = envWithoutTmux()
	return cmd
}

func envWithoutTmux() []string {
	var out []string
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "TMUX=") || strings.HasPrefix(kv, "TMUX_PANE=") {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// startIsolatedServer starts a tmux server on a private -S socket with one
// session and returns the socket path and server pid. Cleanup kills the pid.
func startIsolatedServer(t *testing.T, session string) (string, int) {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	sock, cleanup := testutil.ShortTmuxSocket()
	t.Cleanup(cleanup)
	if out, err := tmuxNoEnv("-S", sock, "new-session", "-d", "-s", session, "sleep 600").CombinedOutput(); err != nil {
		t.Fatalf("start isolated server: %v: %s", err, out)
	}
	out, err := tmuxNoEnv("-S", sock, "display-message", "-p", "#{pid}").Output()
	if err != nil {
		t.Fatalf("server pid: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil || pid <= 1 {
		t.Fatalf("bad server pid %q", out)
	}
	allowSignalPID(pid)
	t.Cleanup(func() {
		_ = syscall.Kill(pid, syscall.SIGTERM)
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			if syscall.Kill(pid, 0) != nil {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		_ = syscall.Kill(pid, syscall.SIGKILL)
	})
	return sock, pid
}

// onlyServers restricts enumeration to the given pids, so a test never even
// considers the user's real servers.
func onlyServers(pids ...int) {
	real := listTmuxServersPS
	listTmuxServers = func() ([]tmuxServerProc, error) {
		all, err := real()
		if err != nil {
			return nil, err
		}
		var out []tmuxServerProc
		for _, s := range all {
			for _, p := range pids {
				if s.PID == p {
					out = append(out, s)
				}
			}
		}
		return out, nil
	}
}

func hasSessionAt(sock, name string) bool {
	return tmuxNoEnv("-S", sock, "has-session", "-t", "="+name).Run() == nil
}

// Deleting a live server's socket file: recovery finds the server (by the
// socket it has bound, which the kernel still reports), SIGUSR1s it, and the
// same server is reachable again with its session intact.
func TestServerGuard_DeletedSocketRecreatedByUSR1(t *testing.T) {
	events := guardTestEnv(t)
	sock, pid := startIsolatedServer(t, "agentdeck_steal1")
	onlyServers(pid)

	if err := os.Remove(sock); err != nil {
		t.Fatal(err)
	}
	if hasSessionAt(sock, "agentdeck_steal1") {
		t.Fatal("precondition: session should be unreachable once the socket is deleted")
	}
	if suspect, _ := socketSuspectAt(sock); !suspect {
		t.Fatal("a missing socket whose server is alive must be suspect")
	}

	res := recoverLostSocketAt(sock)
	if res.action != "usr1-recreated" || res.err != nil {
		t.Fatalf("recovery = %+v, want usr1-recreated", res)
	}
	if !hasSessionAt(sock, "agentdeck_steal1") {
		t.Fatal("session must be reachable again after SIGUSR1")
	}
	gotPID, _, err := sessionsAtPath(sock)
	if err != nil || gotPID != pid {
		t.Fatalf("socket now served by pid %d (err %v), want the original %d", gotPID, err, pid)
	}
	if len(*events) != 1 || (*events)[0].Action != "usr1-recreated" {
		t.Fatalf("want exactly one usr1-recreated notification, got %+v", *events)
	}
}

// The prevention path does the same before a launch: a missing socket with a
// live server is recovered, not replaced by a second server.
func TestServerGuard_PrecheckRecoversMissingSocketInsteadOfStarting(t *testing.T) {
	guardTestEnv(t)
	sock, pid := startIsolatedServer(t, "agentdeck_steal2")
	onlyServers(pid)
	if err := os.Remove(sock); err != nil {
		t.Fatal(err)
	}
	if err := precheckServerStartAt(sock); err != nil {
		t.Fatalf("precheck: %v", err)
	}
	gotPID, names, err := sessionsAtPath(sock)
	if err != nil || gotPID != pid || len(names) != 1 {
		t.Fatalf("after precheck: pid %d names %v err %v; want original pid %d with its session", gotPID, names, err, pid)
	}
}

// staleSocket leaves a socket file nobody listens on: connect() is refused,
// exactly what tmux saw at 11:36:17.
func staleSocket(t *testing.T, path string) {
	t.Helper()
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	l.(*net.UnixListener).SetUnlinkOnClose(false)
	_ = l.Close()
	if classifySocket(path) != socketRefused {
		t.Fatalf("precondition: %s should refuse connections", path)
	}
}

// A refused socket with a live server that may own it: the precheck retries
// with backoff, then refuses the launch. It never returns nil (which would
// hand the socket to tmux's unlink).
func TestServerGuard_RefusedSocketRefusesLaunch(t *testing.T) {
	events := guardTestEnv(t)
	path := filepath.Join(shortTempDir(t), "s")
	staleSocket(t, path)
	listTmuxServers = func() ([]tmuxServerProc, error) {
		return []tmuxServerProc{
			{PID: 900001, Started: time.Now().Add(-time.Hour), Socket: path, SocketExact: true},
			{PID: 900002, Started: time.Now().Add(-time.Minute), Socket: path, SocketExact: true},
		}, nil
	}
	var signalled []int
	signalServer = func(pid int, sig syscall.Signal) error { signalled = append(signalled, pid); return nil }

	err := precheckServerStartAt(path)
	if !errors.Is(err, ErrServerUnreachable) {
		t.Fatalf("precheck = %v, want ErrServerUnreachable", err)
	}
	if !strings.Contains(err.Error(), "tmux server unreachable; socket present") {
		t.Fatalf("error must say what happened: %v", err)
	}
	if len(signalled) != 0 {
		t.Fatalf("with two candidates nothing may be signalled (could take the path from a stalled holder), got %v", signalled)
	}
	if len(*events) != 1 || (*events)[0].Action != "unreachable" {
		t.Fatalf("want one unreachable alert, got %+v", *events)
	}
}

// Session.Start on a refused socket must not run new-session at all.
func TestServerGuard_StartDoesNotSpawnOnRefusedSocket(t *testing.T) {
	guardTestEnv(t)
	name := "adsteal" + strconv.Itoa(os.Getpid())
	path := socketPathFor(name)
	if isUserDefaultTmuxSocketPath(os.Getenv, os.Getuid(), path) {
		t.Fatalf("refusing: %s is the user's default socket", path)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	staleSocket(t, path)
	t.Cleanup(func() { _ = os.Remove(path) })
	before := socketInode(path)

	listTmuxServers = func() ([]tmuxServerProc, error) {
		return []tmuxServerProc{{PID: 900003, Started: time.Now().Add(-time.Hour), Socket: path, SocketExact: true}}, nil
	}
	signalServer = func(int, syscall.Signal) error { return nil } // the fake server never rebinds

	var spawned []string
	prevExec := execCommand
	execCommand = func(name string, arg ...string) *exec.Cmd {
		spawned = append(spawned, name+" "+strings.Join(arg, " "))
		return exec.Command("false")
	}
	t.Cleanup(func() { execCommand = prevExec })

	s := NewSession("steal-guard-start", t.TempDir())
	s.SocketName = name
	err := s.Start("sleep 1")
	if !errors.Is(err, ErrServerUnreachable) {
		t.Fatalf("Start = %v, want ErrServerUnreachable", err)
	}
	for _, c := range spawned {
		if strings.Contains(c, "new-session") {
			t.Fatalf("new-session was attempted: %s", c)
		}
	}
	if socketInode(path) != before {
		t.Fatal("the refused socket file must be left exactly as it was")
	}
}

// A refused socket with no live server is a stale leftover; tmux replacing it
// is correct, so the launch proceeds.
func TestServerGuard_StaleSocketWithoutServerAllowsLaunch(t *testing.T) {
	guardTestEnv(t)
	path := filepath.Join(shortTempDir(t), "s")
	staleSocket(t, path)
	listTmuxServers = func() ([]tmuxServerProc, error) { return nil, nil }
	if err := precheckServerStartAt(path); err != nil {
		t.Fatalf("stale socket without a server must not block launches: %v", err)
	}
}

// Enumeration failure fails closed.
func TestServerGuard_RefusedSocketEnumerationFailureRefuses(t *testing.T) {
	guardTestEnv(t)
	path := filepath.Join(shortTempDir(t), "s")
	staleSocket(t, path)
	listTmuxServers = func() ([]tmuxServerProc, error) { return nil, errors.New("ps failed") }
	if err := precheckServerStartAt(path); !errors.Is(err, ErrServerUnreachable) {
		t.Fatalf("precheck = %v, want ErrServerUnreachable", err)
	}
}

// The steal: a newer, near-empty server holds the path in front of an older
// one with panes. Without agent-deck sessions on the holder, recovery kills
// it (by pid) and SIGUSR1 puts the old server back.
func TestServerGuard_NewerEmptyHolderKilledOldServerRestored(t *testing.T) {
	events := guardTestEnv(t)
	oldSock, oldPID := startIsolatedServer(t, "agentdeck_old")
	for i := 0; i < 2; i++ {
		if out, err := tmuxNoEnv("-S", oldSock, "new-session", "-d", "-s", "agentdeck_old"+strconv.Itoa(i), "sleep 600").CombinedOutput(); err != nil {
			t.Fatalf("%v: %s", err, out)
		}
	}
	if err := os.Remove(oldSock); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1100 * time.Millisecond) // ps start times have 1s resolution
	// What tmux's client_connect does: a new server on the same path.
	if out, err := tmuxNoEnv("-S", oldSock, "new-session", "-d", "-s", "scratch", "sleep 600").CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	newPID, _, err := sessionsAtPath(oldSock)
	if err != nil || newPID == oldPID {
		t.Fatalf("precondition: a second server should hold the path (pid %d, err %v)", newPID, err)
	}
	allowSignalPID(newPID)
	t.Cleanup(func() { _ = syscall.Kill(newPID, syscall.SIGKILL) })
	onlyServers(oldPID, newPID)

	if suspect, reason := socketSuspectAt(oldSock); !suspect {
		t.Fatalf("a newer near-empty holder in front of an older server must be suspect (%s)", reason)
	}
	res := recoverLostSocketAt(oldSock)
	if res.action != "killed-newer-server" {
		t.Fatalf("recovery = %+v (events %+v), want killed-newer-server", res, *events)
	}
	gotPID, names, err := sessionsAtPath(oldSock)
	if err != nil || gotPID != oldPID || len(names) != 3 {
		t.Fatalf("after recovery: pid %d names %v err %v; want old pid %d with 3 sessions", gotPID, names, err, oldPID)
	}
}

// Same steal, but the holder has an agent-deck session: alert, kill nothing,
// signal nothing.
func TestServerGuard_HolderWithAgentDeckSessionOnlyAlerts(t *testing.T) {
	events := guardTestEnv(t)
	oldSock, oldPID := startIsolatedServer(t, "agentdeck_old")
	for i := 0; i < 2; i++ {
		if out, err := tmuxNoEnv("-S", oldSock, "new-session", "-d", "-s", "agentdeck_o"+strconv.Itoa(i), "sleep 600").CombinedOutput(); err != nil {
			t.Fatalf("%v: %s", err, out)
		}
	}
	if err := os.Remove(oldSock); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1100 * time.Millisecond)
	if out, err := tmuxNoEnv("-S", oldSock, "new-session", "-d", "-s", "agentdeck_moved_1", "sleep 600").CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	newPID, _, _ := sessionsAtPath(oldSock)
	allowSignalPID(newPID)
	t.Cleanup(func() { _ = syscall.Kill(newPID, syscall.SIGKILL) })
	onlyServers(oldPID, newPID)
	realSignal := signalServer
	var signalled []int
	signalServer = func(pid int, sig syscall.Signal) error {
		if sig != 0 {
			signalled = append(signalled, pid)
		}
		return realSignal(pid, sig)
	}

	res := recoverLostSocketAt(oldSock)
	if res.action != "alert-known-sessions" {
		t.Fatalf("recovery = %+v, want alert-known-sessions", res)
	}
	if len(signalled) != 0 {
		t.Fatalf("nothing may be signalled while the holder has agent-deck sessions, got %v", signalled)
	}
	if !hasSessionAt(oldSock, "agentdeck_moved_1") {
		t.Fatal("holder must be left alone")
	}
	if len(*events) != 1 || !strings.Contains((*events)[0].Detail, "agentdeck_moved_1") {
		t.Fatalf("want one alert naming the blocking session, got %+v", *events)
	}
}

// Poller side: a vanish on a suspect socket is held and alerts once per
// episode however many sessions vanish; a healthy socket gives a verdict.
func TestServerGuard_AbsenceHeldOnSuspectSocket(t *testing.T) {
	events := guardTestEnv(t)
	path := filepath.Join(shortTempDir(t), "s")
	listTmuxServers = func() ([]tmuxServerProc, error) {
		return []tmuxServerProc{{PID: 900004, Started: time.Now().Add(-time.Hour), Socket: path, SocketExact: true}}, nil
	}
	signalServer = func(int, syscall.Signal) error { return nil }
	for i := 0; i < 5; i++ {
		if !absenceVerdictHeldAt(path, "agentdeck_s"+strconv.Itoa(i)) {
			t.Fatalf("vanish %d on a lost socket must be held", i)
		}
	}
	holding := 0
	for _, ev := range *events {
		if ev.Action == "holding-status" {
			holding++
		}
	}
	if holding != 1 {
		t.Fatalf("want one holding-status alert for the episode, got %d (%+v)", holding, *events)
	}

	resetServerGuardState()
	listTmuxServers = func() ([]tmuxServerProc, error) { return nil, nil }
	if absenceVerdictHeldAt(path, "agentdeck_gone") {
		t.Fatal("missing socket with no server: the session is really gone")
	}
}

// Several sessions vanishing right after the socket file was re-created (a
// different server now) are held even when no older server is visible.
func TestServerGuard_MassVanishAfterSocketRecreatedHeld(t *testing.T) {
	guardTestEnv(t)
	listTmuxServers = func() ([]tmuxServerProc, error) { return nil, nil }
	path := filepath.Join(shortTempDir(t), "s")
	l1, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	observeSocketIdentityAt(path)
	_ = l1.Close()
	l2, err := net.Listen("unix", path) // new server, new socket file
	if err != nil {
		t.Fatal(err)
	}
	defer l2.Close()
	// The raw listener speaks no tmux protocol; answer the holder probe here.
	prevSessions := sessionsAtPath
	sessionsAtPath = func(string) (int, []string, error) { return 0, nil, errors.New("not tmux") }
	t.Cleanup(func() { sessionsAtPath = prevSessions })

	if absenceVerdictHeldAt(path, "agentdeck_a") {
		t.Fatal("a single vanish is a verdict")
	}
	if !absenceVerdictHeldAt(path, "agentdeck_b") {
		t.Fatal("a second vanish right after the socket was re-created must be held")
	}
}

func TestParseTmuxServersPS(t *testing.T) {
	out := strings.Join([]string{
		"  2084     1   501 Tue Oct 20 12:01:16 2026     ??       /opt/homebrew/bin/tmux start-server ; set-option -g exit-empty off",
		"  2090  2084   501 Tue Oct 20 12:01:17 2026     ttys001  -zsh",
		"  2091  2084   501 Tue Oct 20 12:01:17 2026     ttys002  claude",
		"53116 59874   501 Tue Oct 20 13:19:08 2026     ttys002  tmux -u -C attach-session -t x",
		"43452     1   501 Tue Oct 20 11:36:17 2026     ??       tmux -u -L work new-session -d -s y",
		"  777     1     0 Tue Oct 20 11:36:17 2026     ??       tmux new-session",
		"  888     1   501 Tue Oct 20 11:36:17 2026     ?        tmux: server (/tmp/tmux-501/linuxsock)",
	}, "\n")
	env := func(k string) string { return "" }
	got := parseTmuxServersPS(out, 501, env)
	if len(got) != 3 {
		t.Fatalf("want 3 servers (no clients, no other uids), got %+v", got)
	}
	byPID := map[int]tmuxServerProc{}
	for _, s := range got {
		byPID[s.PID] = s
	}
	if s := byPID[2084]; s.Socket != "/tmp/tmux-501/default" || s.Children != 2 {
		t.Fatalf("2084 = %+v", s)
	}
	if s := byPID[43452]; s.Socket != "/tmp/tmux-501/work" {
		t.Fatalf("43452 = %+v", s)
	}
	if s := byPID[888]; s.Socket != "/tmp/tmux-501/linuxsock" {
		t.Fatalf("888 = %+v", s)
	}
	if got[0].PID != 43452 && got[0].PID != 888 {
		t.Fatalf("oldest first: %+v", got)
	}
}

// The signal seam itself refuses unregistered pids in a test binary.
func TestServerGuard_SignalSeamRefusesUnregisteredPID(t *testing.T) {
	if err := signalServer(os.Getppid(), 0); err == nil {
		t.Fatal("a test binary must not signal a pid it did not register")
	}
}
