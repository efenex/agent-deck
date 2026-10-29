package tmux

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/desknotify"
)

// This file guards against the tmux SOCKET STEAL (2026-10-20 incident) and
// recovers from it.
//
// tmux's client_connect() reacts to connect() failing with ECONNREFUSED or
// ENOENT by unlinking the socket path and starting a fresh server, but only for
// a command that may start a server (`new-session`, `start-server`). A live
// server whose listen backlog is momentarily full (macOS answers a full AF_UNIX
// backlog with ECONNREFUSED, not EAGAIN) therefore loses its socket to the
// first `new-session` that arrives during the stall. The old server keeps every
// session but nothing can reach it; the new one holds the path and is empty, so
// every status probe reports every session gone and the poller flips the whole
// fleet to error.
//
// Three defences live here:
//
//  1. Prevent: PrecheckServerStart runs before every server-capable spawn. A
//     socket that exists but refuses connections is retried with backoff and,
//     if a live tmux server could still own it, the spawn is refused rather than
//     handed to tmux (which would unlink it).
//  2. Recover: recoverLostSocketAt detects a missing socket whose server is
//     still alive, or a newer near-empty server holding the path in front of an
//     older one, and repairs it the way `man tmux` documents: SIGUSR1 makes a
//     server re-create its socket.
//  3. Hold: (*Session).AbsenceVerdictHeld tells the status poller that a "not
//     found" reading for a row last seen live is no verdict when the socket is
//     suspect, so it keeps the last status instead of publishing error.
//
// Fit with the status pipeline (#2835/#2880): tmux reads a refusing socket as
// "no server running", so upstream's authoritative listing and its exact
// has-session both prove every session absent during a stall. The hold sits
// where that proof is consumed, beside AbsenceIsForeignServer, and only for
// rows last seen live. It costs no tmux client: a raw connect() and a stat
// decide a healthy socket, ps/lsof run only for a refusing or missing one, and
// the steal verdict (a list-sessions) only after the socket file was replaced.
// Exists() is not touched: "alive on a suspect socket" there answered "already
// running" to fresh launches whenever ps or lsof timed out under load.
//
// Ownership counts only on the kernel's word (lsof, /proc, or Linux tmux's own
// "server (<path>)" title). An argv guess resolves -S/-L under this process's
// environment, not the server's, so it can only make a launch more careful,
// never hold a status or keep a socket file alive.
//
// SIGUSR1 is safe to send to any tmux server: the server unlinks and rebinds
// ITS OWN socket path. Sent to a server that serves some other path it is a
// no-op as far as this path is concerned, which is what lets recovery try each
// candidate in age order without knowing for sure which one owns the path.

// ErrServerUnreachable marks a spawn refused because the socket file is present
// but no server answers on it, while a live tmux server that may own it exists.
// Handing such a spawn to tmux would unlink the socket and orphan that server.
var ErrServerUnreachable = errors.New("tmux server unreachable; socket present")

// socketState classifies a socket path by a raw connect(), without running a
// tmux client (a tmux client is the thing that would unlink it).
type socketState int

const (
	socketReachable socketState = iota
	socketMissing
	socketRefused
	socketTimeout
	socketNotSocket
)

func (s socketState) String() string {
	switch s {
	case socketReachable:
		return "reachable"
	case socketMissing:
		return "missing"
	case socketRefused:
		return "refused"
	case socketTimeout:
		return "timeout"
	case socketNotSocket:
		return "not-socket"
	}
	return "unknown"
}

// socketDialTimeout bounds one raw connect() to a tmux socket. A healthy server
// accepts in well under a millisecond.
var socketDialTimeout = 500 * time.Millisecond

// serverReachBackoff is the retry schedule PrecheckServerStart walks while a
// present socket refuses connections: five attempts, ~7.7s in total, inside the
// 10s budget. A var so tests can shrink it.
var serverReachBackoff = []time.Duration{
	200 * time.Millisecond, 500 * time.Millisecond, time.Second, 2 * time.Second, 4 * time.Second,
}

// guardSleep is time.Sleep behind a seam so tests do not wait out backoffs.
var guardSleep = time.Sleep

// classifySocket probes path with a raw connect(). A successful connection is
// closed at once; tmux treats a client that hangs up before identifying itself
// as gone, with no side effect on the server.
func classifySocket(path string) socketState {
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return socketMissing
		}
		return socketRefused
	}
	if info.Mode()&os.ModeSocket == 0 {
		return socketNotSocket
	}
	conn, err := net.DialTimeout("unix", path, socketDialTimeout)
	if err == nil {
		_ = conn.Close()
		return socketReachable
	}
	switch {
	case errors.Is(err, syscall.ENOENT):
		return socketMissing
	case errors.Is(err, syscall.ECONNREFUSED):
		return socketRefused
	}
	var nerr net.Error
	if errors.As(err, &nerr) && nerr.Timeout() {
		return socketTimeout
	}
	// Anything else (EAGAIN, EACCES, ...) is not a proof that the socket is
	// stale; treat it like a refusal so nothing starts a server over it.
	return socketRefused
}

// socketPathFor resolves where a tmux client for socketName would connect,
// mirroring tmux's own precedence (see resolveTmuxSocketPath).
func socketPathFor(socketName string) string {
	return resolveTmuxSocketPath(os.Getenv, os.Getuid(), socketName, nil)
}

// tmuxServerProc is a live tmux server process of this uid.
type tmuxServerProc struct {
	PID      int
	Started  time.Time
	Children int    // direct children: one per live pane, roughly
	Socket   string // socket path the server serves
	// SocketExact: Socket came from the kernel (lsof, /proc) or from Linux
	// tmux's process title. Otherwise it is guessed from argv and may be wrong.
	SocketExact bool
	Command     string
}

// listTmuxServers enumerates this uid's tmux server processes. A seam: tests
// must never see (or signal) the user's real servers.
var listTmuxServers = listTmuxServersPS

// signalServer delivers sig to pid. A seam for the same reason.
// A test binary may only signal pids it registered with allowSignalPID, so no
// test can reach a real server even through a mis-attributed candidate.
var signalServer = func(pid int, sig syscall.Signal) error {
	if looksLikeGoTestBinary() && !signalAllowed(pid) {
		return fmt.Errorf("tmux socket guard: test binary refused to signal unregistered pid %d", pid)
	}
	return syscall.Kill(pid, sig)
}

var testSignalAllow struct {
	sync.Mutex
	pids map[int]bool
}

func allowSignalPID(pid int) {
	testSignalAllow.Lock()
	defer testSignalAllow.Unlock()
	if testSignalAllow.pids == nil {
		testSignalAllow.pids = make(map[int]bool)
	}
	testSignalAllow.pids[pid] = true
}

func signalAllowed(pid int) bool {
	testSignalAllow.Lock()
	defer testSignalAllow.Unlock()
	return testSignalAllow.pids[pid]
}

// psCommand runs ps. Separate from execCommand, which tests stub for tmux.
var psCommand = func(ctx context.Context, args ...string) ([]byte, error) {
	// #nosec G204 -- fixed binary, fixed argv.
	return exec.CommandContext(ctx, "ps", args...).Output()
}

// listTmuxServersPS finds tmux servers with ps: processes named tmux, owned by
// this uid, with no controlling terminal (a server daemonizes; every client,
// control-mode ones included, keeps the tty it was run from). `pgrep -x tmux`
// is NOT equivalent: on darwin a server started as /opt/homebrew/bin/tmux does
// not match it.
func listTmuxServersPS() ([]tmuxServerProc, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	out, err := psCommand(ctx, "-axo", "pid=,ppid=,uid=,lstart=,tty=,command=")
	if err != nil {
		return nil, fmt.Errorf("ps: %w", err)
	}
	servers := parseTmuxServersPS(string(out), os.Getuid(), os.Getenv)
	pids := make([]int, 0, len(servers))
	for _, s := range servers {
		pids = append(pids, s.PID)
	}
	bound := boundSocketsOf(pids)
	for i := range servers {
		if path := bound[servers[i].PID]; path != "" {
			servers[i].Socket = path
			servers[i].SocketExact = true
		}
	}
	return servers, nil
}

// boundSocketsOf returns, per pid, the path of the unix socket it has bound,
// from the kernel's view (it survives the path being unlinked, which is
// exactly the lost-socket case). Pids it cannot tell are absent. One lsof for
// all pids (darwin and most Linux boxes); Linux without lsof falls back to
// /proc.
var boundSocketsOf = func(pids []int) map[int]string {
	out := make(map[int]string, len(pids))
	if len(pids) == 0 {
		return out
	}
	list := make([]string, 0, len(pids))
	for _, p := range pids {
		list = append(list, strconv.Itoa(p))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	// #nosec G204 -- fixed binary; pids are integers.
	raw, _ := exec.CommandContext(ctx, "lsof", "-a", "-U", "-p", strings.Join(list, ","), "-F", "pn").Output()
	cur := 0
	for _, line := range strings.Split(string(raw), "\n") {
		switch {
		case strings.HasPrefix(line, "p"):
			cur, _ = strconv.Atoi(line[1:])
		case strings.HasPrefix(line, "n/") && cur != 0:
			if _, ok := out[cur]; !ok {
				out[cur] = filepath.Clean(line[1:])
			}
		}
	}
	for _, p := range pids {
		if _, ok := out[p]; !ok {
			if path := boundSocketFromProc(p); path != "" {
				out[p] = path
			}
		}
	}
	return out
}

// boundSocketFromProc maps pid's socket fds to /proc/net/unix paths. Linux
// only; "" elsewhere.
func boundSocketFromProc(pid int) string {
	fdDir := fmt.Sprintf("/proc/%d/fd", pid)
	entries, err := os.ReadDir(fdDir)
	if err != nil {
		return ""
	}
	inodes := make(map[string]bool)
	for _, e := range entries {
		target, err := os.Readlink(filepath.Join(fdDir, e.Name()))
		if err == nil && strings.HasPrefix(target, "socket:[") {
			inodes[strings.TrimSuffix(strings.TrimPrefix(target, "socket:["), "]")] = true
		}
	}
	data, err := os.ReadFile("/proc/net/unix")
	if err != nil {
		return ""
	}
	// Num RefCount Protocol Flags Type St Inode Path; Flags 00010000 = listening.
	for _, line := range strings.Split(string(data), "\n") {
		f := strings.Fields(line)
		if len(f) >= 8 && inodes[f[6]] && f[3] == "00010000" && strings.HasPrefix(f[7], "/") {
			return filepath.Clean(f[7])
		}
	}
	return ""
}

// parseTmuxServersPS parses `ps -axo pid=,ppid=,uid=,lstart=,tty=,command=`.
// lstart is five words ("Tue Oct 20 12:01:16 2026").
func parseTmuxServersPS(out string, uid int, lookupEnv func(string) string) []tmuxServerProc {
	children := make(map[int]int)
	var servers []tmuxServerProc
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) < 10 {
			continue
		}
		pid, err1 := strconv.Atoi(f[0])
		ppid, err2 := strconv.Atoi(f[1])
		puid, err3 := strconv.Atoi(f[2])
		if err1 != nil || err2 != nil || err3 != nil {
			continue
		}
		children[ppid]++
		if puid != uid {
			continue
		}
		tty := f[8]
		if tty != "??" && tty != "?" && tty != "-" {
			continue
		}
		argv := f[9:]
		if filepath.Base(argv[0]) != "tmux" && !strings.HasPrefix(argv[0], "tmux:") {
			continue
		}
		started, err := time.ParseInLocation("Mon Jan 2 15:04:05 2006", strings.Join(f[3:8], " "), time.Local)
		if err != nil {
			continue
		}
		socket, exact := serverSocketFromArgv(argv, uid, lookupEnv)
		servers = append(servers, tmuxServerProc{
			PID:         pid,
			Started:     started,
			Socket:      socket,
			SocketExact: exact,
			Command:     strings.Join(argv, " "),
		})
	}
	for i := range servers {
		servers[i].Children = children[servers[i].PID]
	}
	sort.Slice(servers, func(i, j int) bool {
		if servers[i].Started.Equal(servers[j].Started) {
			return servers[i].PID < servers[j].PID
		}
		return servers[i].Started.Before(servers[j].Started)
	})
	return servers
}

// serverSocketFromArgv recovers a server's socket path. Linux tmux rewrites
// its title to "tmux: server (<path>)", which is exact; darwin keeps the argv
// of the client that forked it, so the -S/-L flags there name the socket. A
// server's $TMUX and TMUX_TMPDIR are not visible, so that resolves under ours:
// a guess (exact false).
func serverSocketFromArgv(argv []string, uid int, lookupEnv func(string) string) (string, bool) {
	joined := strings.Join(argv, " ")
	if i := strings.Index(joined, "server ("); i >= 0 {
		rest := joined[i+len("server ("):]
		if j := strings.LastIndexByte(rest, ')'); j >= 0 {
			return filepath.Clean(rest[:j]), true
		}
	}
	noTmux := func(k string) string {
		if k == "TMUX" {
			return ""
		}
		return lookupEnv(k)
	}
	return resolveTmuxSocketPath(noTmux, uid, "", argv[1:]), false
}

// serversFor returns, oldest first, the servers known to be bound to path
// (owners: SocketExact) and those that might be (unattributed: no exact
// socket, and an argv guess that is empty or names path). Only owners prove a
// live server behind a refusing or missing socket. Enumeration failure is
// reported, so a launch can fail closed.
func serversFor(path string) (owners, unattributed []tmuxServerProc, err error) {
	all, err := listTmuxServers()
	if err != nil {
		return nil, nil, err
	}
	want := normalizeTmpPath(path)
	for _, s := range all {
		match := normalizeTmpPath(s.Socket) == want
		switch {
		case s.SocketExact && match:
			owners = append(owners, s)
		case !s.SocketExact && (s.Socket == "" || match):
			unattributed = append(unattributed, s)
		}
	}
	return owners, unattributed, nil
}

// sessionsAtPath asks the server behind path (explicit -S, so exactly the
// server a spawn would reach) for its pid and session names. list-sessions
// never starts a server. A server with no sessions answers nothing, so the pid
// falls back to display-message.
var sessionsAtPath = func(path string) (int, []string, error) {
	out, err := runBoundedOutput("", "-S", path, "list-sessions", "-F", tmuxFmt("#{pid}", "#{session_name}"))
	pid := 0
	var names []string
	if err == nil {
		for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			parts := strings.SplitN(line, tmuxFieldSep, 2)
			if len(parts) != 2 {
				continue
			}
			if p, perr := strconv.Atoi(parts[0]); perr == nil {
				pid = p
			}
			names = append(names, parts[1])
		}
	}
	if pid == 0 {
		dout, derr := runBoundedOutput("", "-S", path, "display-message", "-p", "#{pid}")
		if derr != nil {
			if err == nil {
				err = derr
			}
			return 0, nil, err
		}
		p, perr := strconv.Atoi(strings.TrimSpace(string(dout)))
		if perr != nil {
			return 0, nil, perr
		}
		pid = p
	}
	return pid, names, nil
}

// ServerRecoveryEvent describes one recovery decision or action, for logs and
// the operator notification.
type ServerRecoveryEvent struct {
	Socket string
	Action string // e.g. "usr1-recreated", "killed-newer-server", "alert-known-sessions"
	Detail string
}

// notifyServerEvent raises the operator notification. A seam: tests replace
// it, and a test binary never reaches a real notifier through the default.
var notifyServerEvent = func(ev ServerRecoveryEvent) {
	if looksLikeGoTestBinary() {
		return
	}
	desknotify.New().NotifyText("agent-deck: tmux socket "+ev.Action, ev.Detail)
}

var serverAlerts struct {
	sync.Mutex
	last map[string]time.Time
}

// serverAlertInterval rate-limits identical alerts: one per episode, not one
// per poll.
var serverAlertInterval = 10 * time.Minute

// reportServerEvent logs every event and notifies at most once per key per
// serverAlertInterval.
func reportServerEvent(ev ServerRecoveryEvent, key string) {
	statusLog.Warn("tmux_socket_guard",
		slog.String("socket", ev.Socket),
		slog.String("action", ev.Action),
		slog.String("detail", ev.Detail))
	serverAlerts.Lock()
	if serverAlerts.last == nil {
		serverAlerts.last = make(map[string]time.Time)
	}
	k := ev.Socket + "|" + ev.Action + "|" + key
	if t, ok := serverAlerts.last[k]; ok && time.Since(t) < serverAlertInterval {
		serverAlerts.Unlock()
		return
	}
	serverAlerts.last[k] = time.Now()
	serverAlerts.Unlock()
	// Off the caller's goroutine: the status poller calls in with i.mu held,
	// and a notifier may take up to its dispatch timeout.
	if reportSync {
		notifyServerEvent(ev)
		return
	}
	go notifyServerEvent(ev)
}

// reportSync delivers notifications inline. Tests only.
var reportSync bool

// PrecheckServerStart must run before any tmux command that can start a server
// on socketName. It returns nil when starting is safe (socket reachable, or no
// live server can own it), and an ErrServerUnreachable error when tmux would
// unlink a socket that a live server may still own.
func PrecheckServerStart(socketName string) error {
	return precheckServerStartAt(socketPathFor(socketName))
}

func precheckServerStartAt(path string) error {
	state := classifySocket(path)
	if state == socketRefused || state == socketTimeout {
		// tmux leaves its socket file behind when the server exits, so a
		// refusing socket is usually just stale. Settle that first (one ps)
		// rather than make every launch after a clean exit wait out the backoff.
		if owners, unknown, err := serversFor(path); err == nil && len(owners)+len(unknown) == 0 {
			statusLog.Info("tmux_socket_stale_no_server", slog.String("socket", path))
			return nil
		}
		for _, d := range serverReachBackoff {
			guardSleep(d)
			if state = classifySocket(path); state != socketRefused && state != socketTimeout {
				break
			}
		}
	}
	switch state {
	case socketReachable, socketNotSocket:
		// not-socket: tmux reports that itself and never unlinks a non-socket.
		return nil
	case socketMissing:
		res := recoverLostSocketAt(path)
		if res.err != nil {
			return res.err
		}
		return nil
	}

	// Still refusing after the backoff. Here an unattributed server counts too:
	// refusing a launch is recoverable, unlinking a live server's socket is not.
	owners, unknown, err := serversFor(path)
	servers := append(owners, unknown...)
	if err == nil && len(servers) == 0 {
		// A socket file with no live server: a stale leftover (a server that was
		// SIGKILLed). tmux unlinking it is correct.
		statusLog.Info("tmux_socket_stale_no_server", slog.String("socket", path))
		return nil
	}
	if err == nil && len(servers) == 1 {
		// The owner may have lost its listener while staying alive; SIGUSR1
		// rebinds it. Harmless to a server that is merely stalled: it rebinds
		// when it runs again. Only with ONE candidate: with several, the one
		// signalled could take the path from a live holder that is stalled.
		if signalCandidatesUntilReachable(path, servers) {
			reportServerEvent(ServerRecoveryEvent{Socket: path, Action: "usr1-recreated",
				Detail: "socket refused connections; SIGUSR1 re-created it"}, "")
			return nil
		}
	}
	pids := make([]string, 0, len(servers))
	for _, s := range servers {
		pids = append(pids, strconv.Itoa(s.PID))
	}
	detail := fmt.Sprintf("%s refused connections for %s; live tmux server pid(s) %s; launch refused so tmux does not unlink it",
		path, totalBackoff(), strings.Join(pids, ","))
	if err != nil {
		detail = fmt.Sprintf("%s refused connections for %s; could not enumerate tmux servers (%v); launch refused", path, totalBackoff(), err)
	}
	reportServerEvent(ServerRecoveryEvent{Socket: path, Action: "unreachable", Detail: detail}, "")
	return fmt.Errorf("%w: %s", ErrServerUnreachable, detail)
}

func totalBackoff() time.Duration {
	var t time.Duration
	for _, d := range serverReachBackoff {
		t += d
	}
	return t
}

// usr1Settle bounds how long recovery waits for a signalled server to rebind.
var usr1Settle = 2 * time.Second

// signalCandidatesUntilReachable sends SIGUSR1 to each server, oldest first,
// until path accepts connections. Returns whether it does.
func signalCandidatesUntilReachable(path string, servers []tmuxServerProc) bool {
	for _, s := range servers {
		if err := signalServer(s.PID, syscall.SIGUSR1); err != nil {
			continue
		}
		statusLog.Warn("tmux_socket_guard_usr1", slog.String("socket", path), slog.Int("pid", s.PID))
		if waitReachable(path, usr1Settle) {
			return true
		}
	}
	return false
}

func waitReachable(path string, budget time.Duration) bool {
	deadline := time.Now().Add(budget)
	for {
		if classifySocket(path) == socketReachable {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		guardSleep(50 * time.Millisecond)
	}
}

type recoveryResult struct {
	action string // "", "healthy", "usr1-recreated", "killed-newer-server", "alert-known-sessions", ...
	err    error  // non-nil only when a spawn must be refused
}

// recoverLostSocketAt detects and repairs a lost socket at path.
//
//   - Missing path with a live server that may own it: SIGUSR1 the oldest
//     candidate (then the next) until the path is back. If none brings it back
//     the candidates served other paths, and starting fresh is safe.
//   - Path held by a server newer than an older live one that has far more
//     panes: the steal. If the holder has no agent-deck sessions it is killed
//     (by pid, re-confirmed, never by path) and the old server is signalled.
//     Otherwise alert and change nothing: SIGUSR1 now would orphan the
//     holder's sessions instead.
func recoverLostSocketAt(path string) recoveryResult {
	state := classifySocket(path)
	switch state {
	case socketMissing:
		// A missing path is the normal state before a first start. Only a
		// server the kernel shows bound to it has lost it; an argv guess would
		// SIGUSR1 unrelated servers on every first launch of an isolated socket.
		servers, _, err := serversFor(path)
		if err != nil {
			// Nothing to unlink, and a server that lost the path rebinds only on
			// SIGUSR1, which nobody sent: starting a new one orphans nothing that
			// is not already lost. Fail open (a ps timeout under load refused
			// every launch).
			statusLog.Warn("tmux_socket_missing_enumeration_failed", slog.String("socket", path), slog.String("error", err.Error()))
			return recoveryResult{}
		}
		if len(servers) == 0 {
			return recoveryResult{}
		}
		if signalCandidatesUntilReachable(path, servers) {
			pid, _, _ := sessionsAtPath(path)
			reportServerEvent(ServerRecoveryEvent{Socket: path, Action: "usr1-recreated",
				Detail: fmt.Sprintf("socket %s was missing; SIGUSR1 made tmux server pid %d re-create it", path, pid)}, "")
			return recoveryResult{action: "usr1-recreated"}
		}
		// The kernel says this live server owns the path. It did not rebind in
		// time (stalled?); it will when it runs, and would then unlink whatever
		// a new-session created now. Refuse.
		srv := servers[0]
		detail := fmt.Sprintf("%s is missing; owner tmux server pid %d is alive but did not re-create it within %s after SIGUSR1",
			path, srv.PID, usr1Settle)
		reportServerEvent(ServerRecoveryEvent{Socket: path, Action: "recovery-failed", Detail: detail}, strconv.Itoa(srv.PID))
		return recoveryResult{action: "recovery-failed", err: fmt.Errorf("%w: %s", ErrServerUnreachable, detail)}
	case socketReachable:
		return recoverStolenSocketAt(path)
	}
	return recoveryResult{action: state.String()}
}

// farFewerPanes reports whether holder looks like the new near-empty server in
// front of old.
func farFewerPanes(holder, old int) bool {
	return old > 2*holder
}

func recoverStolenSocketAt(path string) recoveryResult {
	verdict := stealVerdictAt(path)
	if !verdict.stolen {
		return recoveryResult{action: "healthy"}
	}
	if len(verdict.known) > 0 {
		reportServerEvent(ServerRecoveryEvent{Socket: path, Action: "alert-known-sessions",
			Detail: fmt.Sprintf("%s is held by tmux server pid %d (newer, sessions: %s) in front of older server pid %d (%d panes). "+
				"Not killing: it holds agent-deck sessions. Runbook: move/stop those, then `kill %d; kill -USR1 %d`",
				path, verdict.holder.PID, strings.Join(verdict.known, ","), verdict.old.PID, verdict.old.Children,
				verdict.holder.PID, verdict.old.PID)}, strconv.Itoa(verdict.holder.PID))
		return recoveryResult{action: "alert-known-sessions"}
	}
	// Re-confirm the holder right before killing it, and kill by pid: a
	// `kill-server` on the path could land on the OLD server if another
	// process recovered it in between.
	pid, names, err := sessionsAtPath(path)
	if err != nil || pid != verdict.holder.PID || len(agentDeckNames(names)) > 0 {
		return recoveryResult{action: "holder-changed"}
	}
	if err := signalServer(pid, syscall.SIGTERM); err != nil {
		return recoveryResult{action: "kill-failed"}
	}
	statusLog.Warn("tmux_socket_guard_killed_newer_server", slog.String("socket", path), slog.Int("pid", pid),
		slog.String("sessions", strings.Join(names, ",")))
	waitServerGone(pid, usr1Settle)
	if signalCandidatesUntilReachable(path, []tmuxServerProc{verdict.old}) {
		reportServerEvent(ServerRecoveryEvent{Socket: path, Action: "killed-newer-server",
			Detail: fmt.Sprintf("%s was held by newer tmux server pid %d (sessions: %s); killed it and SIGUSR1 made pid %d re-create the socket",
				path, pid, strings.Join(names, ","), verdict.old.PID)}, strconv.Itoa(pid))
		return recoveryResult{action: "killed-newer-server"}
	}
	reportServerEvent(ServerRecoveryEvent{Socket: path, Action: "recovery-failed",
		Detail: fmt.Sprintf("killed newer server pid %d but older pid %d did not re-create %s; run `kill -USR1 %d`",
			pid, verdict.old.PID, path, verdict.old.PID)}, strconv.Itoa(pid))
	return recoveryResult{action: "recovery-failed"}
}

func waitServerGone(pid int, budget time.Duration) {
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		if err := signalServer(pid, 0); err != nil {
			return
		}
		guardSleep(50 * time.Millisecond)
	}
}

type stealVerdict struct {
	stolen bool
	holder tmuxServerProc
	old    tmuxServerProc
	known  []string // agent-deck sessions on the holder
}

// stealVerdictAt decides whether the server holding path is a newer,
// near-empty server in front of an older live one.
func stealVerdictAt(path string) stealVerdict {
	pid, names, err := sessionsAtPath(path)
	if err != nil || pid == 0 {
		return stealVerdict{}
	}
	all, err := listTmuxServers()
	if err != nil {
		return stealVerdict{}
	}
	var holder *tmuxServerProc
	for i := range all {
		if all[i].PID == pid {
			holder = &all[i]
		}
	}
	if holder == nil {
		return stealVerdict{}
	}
	want := normalizeTmpPath(path)
	for _, s := range all { // oldest first
		if s.PID == pid || !s.Started.Before(holder.Started) {
			continue
		}
		if !s.SocketExact || normalizeTmpPath(s.Socket) != want {
			continue
		}
		if !farFewerPanes(holder.Children, s.Children) {
			continue
		}
		return stealVerdict{stolen: true, holder: *holder, old: s, known: agentDeckNames(names)}
	}
	return stealVerdict{}
}

// agentDeckNames filters the session names agent-deck owns.
func agentDeckNames(names []string) []string {
	var out []string
	for _, n := range names {
		if strings.HasPrefix(n, SessionPrefix) {
			out = append(out, n)
		}
	}
	return out
}

// --- status-poller side -----------------------------------------------------

// socketHealthTTL caches a socket's suspect verdict so a pass that sees many
// sessions vanish probes the socket once, not once per session.
var socketHealthTTL = 2 * time.Second

type socketHealthEntry struct {
	at      time.Time
	suspect bool
	reason  string
	// stealIno/stealAt: the socket inode the (expensive: list-sessions + ps)
	// steal verdict was computed for. A steal always binds a new socket file,
	// so the verdict is reused while the inode is unchanged, up to
	// stealVerdictTTL.
	stealIno uint64
	stealAt  time.Time
}

// stealVerdictTTL bounds reuse of a steal verdict for an unchanged socket inode.
var stealVerdictTTL = time.Minute

var socketHealth struct {
	sync.Mutex
	byPath map[string]socketHealthEntry
	// identity tracks the socket file's inode: a new server (or a SIGUSR1)
	// binds a new file, so a changed inode means the server behind the path may
	// have changed. Free to observe (one stat), unlike a pid probe.
	ino        map[string]uint64
	inoChanged map[string]time.Time
	vanished   map[string]map[string]time.Time
	// firstVanish: when each session was FIRST seen vanished in the window.
	firstVanish map[string]map[string]time.Time
	recovering  map[string]time.Time
}

// massVanishWindow is how recently sessions must have vanished, and the socket
// identity changed, for a multi-session vanish to be held.
var massVanishWindow = 2 * time.Minute

// recoveryCooldown bounds how often the poller kicks recovery per socket.
var recoveryCooldown = 30 * time.Second

// runRecoveryAsync runs recovery off the status goroutine. A seam so tests can
// run it inline. In a test binary the default runs nothing: recovery's
// list-sessions would land in another test's tmux subprocess budget, and its
// signals are refused there anyway; the guard's own tests install a runner.
var runRecoveryAsync = func(fn func()) {
	if looksLikeGoTestBinary() {
		return
	}
	go fn()
}

// ObserveSocketIdentity records the inode of socketName's socket file. Called
// on the poller's refresh tick so a later mass vanish can be compared with the
// identity the sessions were last seen under.
func ObserveSocketIdentity(socketName string) {
	observeSocketIdentityAt(socketPathFor(socketName))
}

func observeSocketIdentityAt(path string) {
	ino := socketInode(path)
	socketHealth.Lock()
	defer socketHealth.Unlock()
	if socketHealth.ino == nil {
		socketHealth.ino = make(map[string]uint64)
		socketHealth.inoChanged = make(map[string]time.Time)
	}
	// Only a socket file REPLACED by another counts: a new server binding a
	// path that had no socket (the first start, or a start after the last
	// server exited) is not a steal, and a socket disappearing is handled as
	// "missing" elsewhere.
	if prev, ok := socketHealth.ino[path]; ok && prev != 0 && ino != 0 && prev != ino {
		socketHealth.inoChanged[path] = time.Now()
	}
	socketHealth.ino[path] = ino
}

// AbsenceVerdictHeld reports whether a "session not found" reading for s must
// NOT be published, because the socket it came from is suspect: unreachable,
// missing while a server that may own it is alive, held by a newer server in
// front of an older one, or several sessions vanished right after the socket's
// identity changed. The caller keeps the last status. Recovery is kicked in the
// background and the operator alerted once.
func (s *Session) AbsenceVerdictHeld() bool {
	return absenceVerdictHeldAt(socketPathFor(s.SocketName), s.Name)
}

func absenceVerdictHeldAt(path, name string) bool {
	// Observe the socket file before recording the vanish, so a replacement
	// the refresh tick has not seen yet still dates before this vanish.
	observeSocketIdentityAt(path)
	suspect, reason := socketSuspectAt(path)
	vanishedCount := noteVanish(path, name)
	if !suspect {
		if vanishedCount < 2 || !identityChangedRecently(path) || vanishedSinceIdentityChange(path) < 2 {
			return false
		}
		reason = fmt.Sprintf("%d sessions vanished within %s of the socket being re-created", vanishedCount, massVanishWindow)
	}
	reportServerEvent(ServerRecoveryEvent{Socket: path, Action: "holding-status",
		Detail: "not marking vanished sessions error: " + reason}, "")
	kickRecovery(path)
	return true
}

func noteVanish(path, name string) int {
	socketHealth.Lock()
	defer socketHealth.Unlock()
	if socketHealth.vanished == nil {
		socketHealth.vanished = make(map[string]map[string]time.Time)
	}
	m := socketHealth.vanished[path]
	if m == nil {
		m = make(map[string]time.Time)
		socketHealth.vanished[path] = m
	}
	now := time.Now()
	m[name] = now
	if socketHealth.firstVanish == nil {
		socketHealth.firstVanish = make(map[string]map[string]time.Time)
	}
	fv := socketHealth.firstVanish[path]
	if fv == nil {
		fv = make(map[string]time.Time)
		socketHealth.firstVanish[path] = fv
	}
	if _, seen := fv[name]; !seen {
		fv[name] = now
	}
	for k, t := range fv {
		if now.Sub(t) > massVanishWindow {
			delete(fv, k)
		}
	}
	n := 0
	for k, t := range m {
		if now.Sub(t) > massVanishWindow {
			delete(m, k)
			continue
		}
		n++
	}
	return n
}

// vanishedSinceIdentityChange counts sessions first seen vanished after the
// socket file was last replaced. Sessions that were already gone before (a
// stopped session re-probed) are not part of a mass vanish.
func vanishedSinceIdentityChange(path string) int {
	socketHealth.Lock()
	defer socketHealth.Unlock()
	changed := socketHealth.inoChanged[path]
	n := 0
	for _, t := range socketHealth.firstVanish[path] {
		if !t.Before(changed) {
			n++
		}
	}
	return n
}

func identityChangedRecently(path string) bool {
	observeSocketIdentityAt(path)
	socketHealth.Lock()
	defer socketHealth.Unlock()
	t, ok := socketHealth.inoChanged[path]
	return ok && time.Since(t) < massVanishWindow
}

func socketSuspectAt(path string) (bool, string) {
	if o := socketSuspectOverride; o != nil {
		return o(path)
	}
	socketHealth.Lock()
	if socketHealth.byPath == nil {
		socketHealth.byPath = make(map[string]socketHealthEntry)
	}
	prev, ok := socketHealth.byPath[path]
	socketHealth.Unlock()
	if ok && time.Since(prev.at) < socketHealthTTL {
		return prev.suspect, prev.reason
	}

	e := computeSocketSuspect(path, prev)
	socketHealth.Lock()
	socketHealth.byPath[path] = e
	socketHealth.Unlock()
	return e.suspect, e.reason
}

func socketInode(path string) uint64 {
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		return uint64(st.Ino)
	}
	return 0
}

func computeSocketSuspect(path string, prev socketHealthEntry) socketHealthEntry {
	now := time.Now()
	e := socketHealthEntry{at: now}
	switch state := classifySocket(path); state {
	case socketRefused, socketTimeout, socketMissing:
		// Suspect only on positive evidence: a live server the kernel shows
		// bound to this path. A socket left behind by a server that exited
		// refuses too, and a failed enumeration proves nothing; in both cases
		// the absence stands.
		owners, _, err := serversFor(path)
		if err != nil || len(owners) == 0 {
			return e
		}
		e.suspect, e.reason = true, fmt.Sprintf("socket %s is %s while tmux server pid %d is bound to it", path, state, owners[0].PID)
	case socketReachable:
		// A steal always binds a new socket file. Without a recent
		// replacement a reachable socket is healthy, decided without the
		// list-sessions the steal verdict costs (upstream's dead-row budget is
		// one listing and one has-session per row).
		if !identityChangedRecently(path) {
			return e
		}
		ino := socketInode(path)
		if ino != 0 && prev.stealIno == ino && now.Sub(prev.stealAt) < stealVerdictTTL {
			e.stealIno, e.stealAt = prev.stealIno, prev.stealAt
			if prev.suspect {
				e.suspect, e.reason = true, prev.reason
			}
			return e
		}
		e.stealIno, e.stealAt = ino, now
		if v := stealVerdictAt(path); v.stolen {
			e.suspect, e.reason = true, fmt.Sprintf("socket %s is held by newer tmux server pid %d (%d panes) in front of older pid %d (%d panes)",
				path, v.holder.PID, v.holder.Children, v.old.PID, v.old.Children)
		}
	}
	return e
}

func kickRecovery(path string) {
	socketHealth.Lock()
	if socketHealth.recovering == nil {
		socketHealth.recovering = make(map[string]time.Time)
	}
	if t, ok := socketHealth.recovering[path]; ok && time.Since(t) < recoveryCooldown {
		socketHealth.Unlock()
		return
	}
	socketHealth.recovering[path] = time.Now()
	socketHealth.Unlock()
	runRecoveryAsync(func() {
		res := recoverLostSocketAt(path)
		statusLog.Info("tmux_socket_guard_recovery_pass", slog.String("socket", path), slog.String("action", res.action))
		// Drop the cached verdict so the next poll re-probes the repaired socket.
		socketHealth.Lock()
		delete(socketHealth.byPath, path)
		socketHealth.Unlock()
	})
}

// resetSocketHealthCache drops cached socket verdicts only. Tests only.
func resetSocketHealthCache() {
	socketHealth.Lock()
	socketHealth.byPath = nil
	socketHealth.Unlock()
}

// resetServerGuardState clears all cached guard state. Tests only.
func resetServerGuardState() {
	socketHealth.Lock()
	socketHealth.byPath = nil
	socketHealth.ino = nil
	socketHealth.inoChanged = nil
	socketHealth.vanished = nil
	socketHealth.firstVanish = nil
	socketHealth.recovering = nil
	socketHealth.Unlock()
	serverAlerts.Lock()
	serverAlerts.last = nil
	serverAlerts.Unlock()
}

// socketSuspectOverride replaces the socket verdict. Tests only.
var socketSuspectOverride func(path string) (bool, string)

// SetSocketGuardForTest lets tests in other packages force the suspect-socket
// verdict and capture operator alerts, with recovery disabled. It returns a
// restore func. Never call it outside tests.
func SetSocketGuardForTest(suspect func(path string) (bool, string), notify func(ServerRecoveryEvent)) (restore func()) {
	prevSuspect, prevNotify, prevAsync := socketSuspectOverride, notifyServerEvent, runRecoveryAsync
	socketSuspectOverride = suspect
	if notify != nil {
		notifyServerEvent = notify
	}
	runRecoveryAsync = func(func()) {}
	reportSync = true
	resetServerGuardState()
	return func() {
		socketSuspectOverride, notifyServerEvent, runRecoveryAsync = prevSuspect, prevNotify, prevAsync
		reportSync = false
		resetServerGuardState()
	}
}
